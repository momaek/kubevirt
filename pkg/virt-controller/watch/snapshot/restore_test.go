package snapshot

import (
	"context"
	"fmt"

	"github.com/golang/mock/gomock"
	vsv1 "github.com/kubernetes-csi/external-snapshotter/client/v4/apis/volumesnapshot/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	framework "k8s.io/client-go/tools/cache/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/pointer"

	v1 "kubevirt.io/api/core/v1"
	snapshotv1 "kubevirt.io/api/snapshot/v1alpha1"
	cdifake "kubevirt.io/client-go/generated/containerized-data-importer/clientset/versioned/fake"
	kubevirtfake "kubevirt.io/client-go/generated/kubevirt/clientset/versioned/fake"
	"kubevirt.io/client-go/kubecli"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"

	virtcontroller "kubevirt.io/kubevirt/pkg/controller"
	"kubevirt.io/kubevirt/pkg/testutils"
	"kubevirt.io/kubevirt/pkg/util/status"
)

var _ = Describe("Restore controlleer", func() {
	const (
		testNamespace  = "default"
		uid            = "uid"
		vmName         = "testvm"
		vmSnapshotName = "snapshot"
	)

	var (
		vmAPIGroup       = "kubevirt.io"
		timeStamp        = metav1.Now()
		storageClassName = "sc"
		vmRestoreName    = "restore"
	)

	timeFunc := func() *metav1.Time {
		return &timeStamp
	}

	createRestore := func() *snapshotv1.VirtualMachineRestore {
		return &snapshotv1.VirtualMachineRestore{
			ObjectMeta: metav1.ObjectMeta{
				Name:      vmRestoreName,
				Namespace: testNamespace,
				UID:       uid,
			},
			Status: &snapshotv1.VirtualMachineRestoreStatus{},
			Spec: snapshotv1.VirtualMachineRestoreSpec{
				Target: corev1.TypedLocalObjectReference{
					APIGroup: &vmAPIGroup,
					Kind:     "VirtualMachine",
					Name:     vmName,
				},
				VirtualMachineSnapshotName: vmSnapshotName,
			},
		}
	}

	createRestoreWithOwner := func() *snapshotv1.VirtualMachineRestore {
		r := createRestore()
		r.OwnerReferences = []metav1.OwnerReference{
			{
				APIVersion:         v1.GroupVersion.String(),
				Kind:               "VirtualMachine",
				Name:               vmName,
				UID:                vmUID,
				Controller:         &t,
				BlockOwnerDeletion: &t,
			},
		}
		r.Status = &snapshotv1.VirtualMachineRestoreStatus{
			Complete: &f,
		}
		return r
	}

	addVolumeRestores := func(r *snapshotv1.VirtualMachineRestore) {
		r.Status.Restores = []snapshotv1.VolumeRestore{
			{
				VolumeName:                "disk1",
				PersistentVolumeClaimName: "restore-uid-disk1",
				VolumeSnapshotName:        "vmsnapshot-snapshot-uid-volume-disk1",
			},
		}
	}

	getRestorePVCs := func(vmr *snapshotv1.VirtualMachineRestore) []corev1.PersistentVolumeClaim {
		var pvcs []corev1.PersistentVolumeClaim
		for _, r := range vmr.Status.Restores {
			pvc := corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:   testNamespace,
					Name:        r.PersistentVolumeClaimName,
					Annotations: map[string]string{"restore.kubevirt.io/name": vmr.Name},
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					StorageClassName: &storageClassName,
				},
			}
			pvcs = append(pvcs, pvc)
		}
		return pvcs
	}

	createSnapshot := func() *snapshotv1.VirtualMachineSnapshot {
		s := createVirtualMachineSnapshot(testNamespace, vmSnapshotName, vmName)
		s.Finalizers = []string{"snapshot.kubevirt.io/vmsnapshot-protection"}
		s.Status = &snapshotv1.VirtualMachineSnapshotStatus{
			ReadyToUse:   &t,
			CreationTime: timeFunc(),
			SourceUID:    &vmUID,
		}
		return s
	}

	createSnapshotVM := func() *v1.VirtualMachine {
		return createVirtualMachine(testNamespace, vmName)
	}

	createModifiedVM := func() *v1.VirtualMachine {
		vm := createVirtualMachine(testNamespace, vmName)
		vm.Spec.Template.Spec.Domain.Resources.Requests[corev1.ResourceName(corev1.ResourceRequestsMemory)] = resource.MustParse("128M")
		return vm
	}

	createVMI := func(vm *v1.VirtualMachine) *v1.VirtualMachineInstance {
		return &v1.VirtualMachineInstance{
			ObjectMeta: metav1.ObjectMeta{
				Name:      vm.Name,
				Namespace: vm.Namespace,
			},
		}
	}

	getDeletedDataVolumes := func(vm *v1.VirtualMachine) []string {
		var names []string
		for _, dvt := range vm.Spec.DataVolumeTemplates {
			names = append(names, dvt.Name)
		}
		return names
	}

	createStorageClass := func() *storagev1.StorageClass {
		bm := storagev1.VolumeBindingImmediate
		return &storagev1.StorageClass{
			ObjectMeta: metav1.ObjectMeta{
				Name: storageClassName,
			},
			VolumeBindingMode: &bm,
		}
	}

	createVolumeSnapshot := func(name string, restoreSize resource.Quantity) *vsv1.VolumeSnapshot {
		return &vsv1.VolumeSnapshot{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
			Status: &vsv1.VolumeSnapshotStatus{
				RestoreSize: &restoreSize,
			},
		}
	}

	Context("One valid Restore controller given", func() {

		var ctrl *gomock.Controller

		var vmInterface *kubecli.MockVirtualMachineInterface

		var vmRestoreSource *framework.FakeControllerSource
		var vmRestoreInformer cache.SharedIndexInformer

		var vmSnapshotSource *framework.FakeControllerSource
		var vmSnapshotInformer cache.SharedIndexInformer

		var vmSnapshotContentSource *framework.FakeControllerSource
		var vmSnapshotContentInformer cache.SharedIndexInformer

		var vmInformer cache.SharedIndexInformer
		var vmSource *framework.FakeControllerSource

		var vmiInformer cache.SharedIndexInformer
		var vmiSource *framework.FakeControllerSource

		var dataVolumeInformer cache.SharedIndexInformer
		var dataVolumeSource *framework.FakeControllerSource

		var pvcInformer cache.SharedIndexInformer
		var pvcSource *framework.FakeControllerSource

		var storageClassInformer cache.SharedIndexInformer
		var storageClassSource *framework.FakeControllerSource

		var stop chan struct{}
		var controller *VMRestoreController
		var recorder *record.FakeRecorder
		var mockVMRestoreQueue *testutils.MockWorkQueue
		var fakeVolumeSnapshotProvider *MockVolumeSnapshotProvider

		var kubevirtClient *kubevirtfake.Clientset
		var k8sClient *k8sfake.Clientset
		var cdiClient *cdifake.Clientset

		syncCaches := func(stop chan struct{}) {
			go vmRestoreInformer.Run(stop)
			go vmSnapshotInformer.Run(stop)
			go vmSnapshotContentInformer.Run(stop)
			go vmInformer.Run(stop)
			go pvcInformer.Run(stop)
			go vmiInformer.Run(stop)
			go dataVolumeInformer.Run(stop)
			go storageClassInformer.Run(stop)
			Expect(cache.WaitForCacheSync(
				stop,
				vmRestoreInformer.HasSynced,
				vmSnapshotInformer.HasSynced,
				vmSnapshotContentInformer.HasSynced,
				vmInformer.HasSynced,
				pvcInformer.HasSynced,
				vmiInformer.HasSynced,
				dataVolumeInformer.HasSynced,
				storageClassInformer.HasSynced,
			)).To(BeTrue())
		}

		BeforeEach(func() {
			stop = make(chan struct{})
			ctrl = gomock.NewController(GinkgoT())
			virtClient := kubecli.NewMockKubevirtClient(ctrl)
			vmInterface = kubecli.NewMockVirtualMachineInterface(ctrl)

			vmRestoreInformer, vmRestoreSource = testutils.NewFakeInformerWithIndexersFor(&snapshotv1.VirtualMachineRestore{}, virtcontroller.GetVirtualMachineRestoreInformerIndexers())
			vmSnapshotInformer, vmSnapshotSource = testutils.NewFakeInformerFor(&snapshotv1.VirtualMachineSnapshot{})
			vmSnapshotContentInformer, vmSnapshotContentSource = testutils.NewFakeInformerFor(&snapshotv1.VirtualMachineSnapshotContent{})
			vmiInformer, vmiSource = testutils.NewFakeInformerFor(&v1.VirtualMachineInstance{})
			vmInformer, vmSource = testutils.NewFakeInformerFor(&v1.VirtualMachine{})
			dataVolumeInformer, dataVolumeSource = testutils.NewFakeInformerFor(&cdiv1.DataVolume{})
			pvcInformer, pvcSource = testutils.NewFakeInformerFor(&corev1.PersistentVolumeClaim{})
			storageClassInformer, storageClassSource = testutils.NewFakeInformerFor(&storagev1.StorageClass{})

			recorder = record.NewFakeRecorder(100)
			recorder.IncludeObject = true

			fakeVolumeSnapshotProvider = &MockVolumeSnapshotProvider{
				volumeSnapshots: []*vsv1.VolumeSnapshot{},
			}

			controller = &VMRestoreController{
				Client:                    virtClient,
				VMRestoreInformer:         vmRestoreInformer,
				VMSnapshotInformer:        vmSnapshotInformer,
				VMSnapshotContentInformer: vmSnapshotContentInformer,
				VMInformer:                vmInformer,
				VMIInformer:               vmiInformer,
				PVCInformer:               pvcInformer,
				StorageClassInformer:      storageClassInformer,
				DataVolumeInformer:        dataVolumeInformer,
				Recorder:                  recorder,
				vmStatusUpdater:           status.NewVMStatusUpdater(virtClient),
				VolumeSnapshotProvider:    fakeVolumeSnapshotProvider,
			}
			controller.Init()

			// Wrap our workqueue to have a way to detect when we are done processing updates
			mockVMRestoreQueue = testutils.NewMockWorkQueue(controller.vmRestoreQueue)
			controller.vmRestoreQueue = mockVMRestoreQueue

			// Set up mock client
			virtClient.EXPECT().VirtualMachine(testNamespace).Return(vmInterface).AnyTimes()

			kubevirtClient = kubevirtfake.NewSimpleClientset()

			virtClient.EXPECT().VirtualMachineRestore(testNamespace).
				Return(kubevirtClient.SnapshotV1alpha1().VirtualMachineRestores(testNamespace)).AnyTimes()
			virtClient.EXPECT().VirtualMachineSnapshot(testNamespace).
				Return(kubevirtClient.SnapshotV1alpha1().VirtualMachineSnapshots(testNamespace)).AnyTimes()
			virtClient.EXPECT().VirtualMachineSnapshotContent(testNamespace).
				Return(kubevirtClient.SnapshotV1alpha1().VirtualMachineSnapshotContents(testNamespace)).AnyTimes()

			k8sClient = k8sfake.NewSimpleClientset()
			virtClient.EXPECT().CoreV1().Return(k8sClient.CoreV1()).AnyTimes()

			cdiClient = cdifake.NewSimpleClientset()
			virtClient.EXPECT().CdiClient().Return(cdiClient).AnyTimes()

			k8sClient.Fake.PrependReactor("*", "*", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
				Expect(action).To(BeNil())
				return true, nil, nil
			})
			kubevirtClient.Fake.PrependReactor("*", "*", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
				Expect(action).To(BeNil())
				return true, nil, nil
			})

			currentTime = timeFunc
		})

		addVirtualMachineRestore := func(r *snapshotv1.VirtualMachineRestore) {
			syncCaches(stop)
			mockVMRestoreQueue.ExpectAdds(1)
			vmRestoreSource.Add(r)
			mockVMRestoreQueue.Wait()
		}

		addPVC := func(pvc *corev1.PersistentVolumeClaim) {
			syncCaches(stop)
			mockVMRestoreQueue.ExpectAdds(1)
			pvcSource.Add(pvc)
			mockVMRestoreQueue.Wait()
		}

		addVM := func(vm *v1.VirtualMachine) {
			syncCaches(stop)
			mockVMRestoreQueue.ExpectAdds(1)
			vmSource.Add(vm)
			mockVMRestoreQueue.Wait()
		}

		expectUpdateVMRestoreInProgress := func(vm *v1.VirtualMachine) {
			vmStatusUpdate := vm.DeepCopy()
			vmStatusUpdate.ResourceVersion = "1"
			vmStatusUpdate.Status.RestoreInProgress = &vmRestoreName
			vmInterface.EXPECT().UpdateStatus(vmStatusUpdate).Return(vmStatusUpdate, nil)
		}

		Context("with initialized snapshot and content", func() {

			var (
				s            *snapshotv1.VirtualMachineSnapshot
				vm           *v1.VirtualMachine
				sc           *snapshotv1.VirtualMachineSnapshotContent
				storageClass *storagev1.StorageClass
			)

			BeforeEach(func() {
				s = createSnapshot()
				vm = createSnapshotVM()
				pvcs := createPVCsForVM(vm)
				sc = createVirtualMachineSnapshotContent(s, vm, pvcs)
				storageClass = createStorageClass()
				s.Status.VirtualMachineSnapshotContentName = &sc.Name
				sc.Status = &snapshotv1.VirtualMachineSnapshotContentStatus{
					CreationTime: timeFunc(),
					ReadyToUse:   &t,
				}
				vmSnapshotSource.Add(s)
				vmSnapshotContentSource.Add(sc)
				storageClassSource.Add(storageClass)
			})

			It("should error if snapshot does not exist", func() {
				r := createRestoreWithOwner()
				vm := createModifiedVM()
				rc := r.DeepCopy()
				rc.ResourceVersion = "1"
				rc.Status = &snapshotv1.VirtualMachineRestoreStatus{
					Complete: &f,
					Conditions: []snapshotv1.Condition{
						newProgressingCondition(corev1.ConditionFalse, "VMSnapshot default/snapshot does not exist"),
						newReadyCondition(corev1.ConditionFalse, "VMSnapshot default/snapshot does not exist"),
					},
				}
				vmSnapshotSource.Delete(createSnapshot())
				vmSource.Add(vm)
				expectUpdateVMRestoreInProgress(vm)
				expectVMRestoreUpdate(kubevirtClient, rc)
				addVirtualMachineRestore(r)
				controller.processVMRestoreWorkItem()
				testutils.ExpectEvent(recorder, "VirtualMachineRestoreError")
			})

			It("should update restore status, initializing conditions and add owner", func() {
				r := createRestoreWithOwner()
				refs := r.OwnerReferences
				r.OwnerReferences = nil
				vm := createModifiedVM()
				rc := r.DeepCopy()
				rc.OwnerReferences = refs
				rc.ResourceVersion = "1"
				rc.Status = &snapshotv1.VirtualMachineRestoreStatus{
					Complete: &f,
					Conditions: []snapshotv1.Condition{
						newProgressingCondition(corev1.ConditionTrue, "Initializing VirtualMachineRestore"),
						newReadyCondition(corev1.ConditionFalse, "Initializing VirtualMachineRestore"),
					},
				}
				vmSource.Add(vm)
				expectUpdateVMRestoreInProgress(vm)
				expectVMRestoreUpdate(kubevirtClient, rc)
				addVirtualMachineRestore(r)
				controller.processVMRestoreWorkItem()
			})

			It("should update restore status with condition and VolumeRestores", func() {
				r := createRestoreWithOwner()
				vm := createModifiedVM()
				rc := r.DeepCopy()
				rc.ResourceVersion = "1"
				rc.Status = &snapshotv1.VirtualMachineRestoreStatus{
					Complete: &f,
					Conditions: []snapshotv1.Condition{
						newProgressingCondition(corev1.ConditionTrue, "Creating new PVCs"),
						newReadyCondition(corev1.ConditionFalse, "Waiting for new PVCs"),
					},
				}
				vmSource.Add(vm)
				expectUpdateVMRestoreInProgress(vm)
				addVolumeRestores(rc)
				expectVMRestoreUpdate(kubevirtClient, rc)
				addVirtualMachineRestore(r)
				controller.processVMRestoreWorkItem()
			})

			It("should create restore PVCs", func() {
				r := createRestoreWithOwner()
				vm := createModifiedVM()
				r.Status = &snapshotv1.VirtualMachineRestoreStatus{
					Complete: &f,
					Conditions: []snapshotv1.Condition{
						newProgressingCondition(corev1.ConditionTrue, "Creating new PVCs"),
						newReadyCondition(corev1.ConditionFalse, "Waiting for new PVCs"),
					},
				}
				vmSource.Add(vm)
				addVolumeRestores(r)
				pvcSize := resource.MustParse("2Gi")
				vs := createVolumeSnapshot(r.Status.Restores[0].VolumeSnapshotName, pvcSize)
				fakeVolumeSnapshotProvider.Add(vs)
				expectUpdateVMRestoreInProgress(vm)
				expectPVCCreates(k8sClient, r, pvcSize)
				addVirtualMachineRestore(r)
				controller.processVMRestoreWorkItem()
			})

			It("should create restore PVC with volume snapshot size if bigger then PVC size", func() {
				r := createRestoreWithOwner()
				vm := createModifiedVM()
				r.Status = &snapshotv1.VirtualMachineRestoreStatus{
					Complete: &f,
					Conditions: []snapshotv1.Condition{
						newProgressingCondition(corev1.ConditionTrue, "Creating new PVCs"),
						newReadyCondition(corev1.ConditionFalse, "Waiting for new PVCs"),
					},
				}
				vmSource.Add(vm)
				addVolumeRestores(r)
				q := resource.MustParse("3Gi")
				vs := createVolumeSnapshot(r.Status.Restores[0].VolumeSnapshotName, q)
				fakeVolumeSnapshotProvider.Add(vs)
				expectUpdateVMRestoreInProgress(vm)
				expectPVCCreates(k8sClient, r, q)
				addVirtualMachineRestore(r)
				controller.processVMRestoreWorkItem()
			})

			It("should create restore PVC with pvc size if restore size is smaller", func() {
				r := createRestoreWithOwner()
				vm := createModifiedVM()
				r.Status = &snapshotv1.VirtualMachineRestoreStatus{
					Complete: &f,
					Conditions: []snapshotv1.Condition{
						newProgressingCondition(corev1.ConditionTrue, "Creating new PVCs"),
						newReadyCondition(corev1.ConditionFalse, "Waiting for new PVCs"),
					},
				}
				vmSource.Add(vm)
				addVolumeRestores(r)
				q := resource.MustParse("1Gi")
				vs := createVolumeSnapshot(r.Status.Restores[0].VolumeSnapshotName, q)
				fakeVolumeSnapshotProvider.Add(vs)
				expectUpdateVMRestoreInProgress(vm)
				pvcSize := resource.MustParse("2Gi")
				expectPVCCreates(k8sClient, r, pvcSize)
				addVirtualMachineRestore(r)
				controller.processVMRestoreWorkItem()
			})

			It("should wait for bound", func() {
				r := createRestoreWithOwner()
				r.Status = &snapshotv1.VirtualMachineRestoreStatus{
					Complete: &f,
					Conditions: []snapshotv1.Condition{
						newProgressingCondition(corev1.ConditionTrue, "Creating new PVCs"),
						newReadyCondition(corev1.ConditionFalse, "Waiting for new PVCs"),
					},
				}
				addVolumeRestores(r)

				vm := createModifiedVM()
				vmi := createVMI(vm)
				vmSource.Add(vm)
				vmiSource.Add(vmi)
				vmRestoreSource.Add(r)
				for _, pvc := range getRestorePVCs(r) {
					pvc.Status.Phase = corev1.ClaimPending
					addPVC(&pvc)
				}
				expectUpdateVMRestoreInProgress(vm)
				controller.processVMRestoreWorkItem()
			})

			It("should update restore status with datavolume", func() {
				r := createRestoreWithOwner()
				r.Status = &snapshotv1.VirtualMachineRestoreStatus{
					Complete: &f,
					Conditions: []snapshotv1.Condition{
						newProgressingCondition(corev1.ConditionTrue, "Creating new PVCs"),
						newReadyCondition(corev1.ConditionFalse, "Waiting for new PVCs"),
					},
				}
				addVolumeRestores(r)
				ur := r.DeepCopy()
				ur.ResourceVersion = "1"
				ur.Status.Conditions = []snapshotv1.Condition{
					newProgressingCondition(corev1.ConditionFalse, "Waiting for target to be ready"),
					newReadyCondition(corev1.ConditionFalse, "Waiting for target to be ready"),
				}

				vm := createModifiedVM()
				vmi := createVMI(vm)
				vmSource.Add(vm)
				vmiSource.Add(vmi)
				vmRestoreSource.Add(r)
				expectUpdateVMRestoreInProgress(vm)
				expectVMRestoreUpdate(kubevirtClient, ur)
				for _, pvc := range getRestorePVCs(r) {
					pvc.Status.Phase = corev1.ClaimBound
					addPVC(&pvc)
				}
				controller.processVMRestoreWorkItem()
			})

			It("should update PVCs and restores to have datavolumename", func() {
				r := createRestoreWithOwner()
				r.Status = &snapshotv1.VirtualMachineRestoreStatus{
					Complete: &f,
					Conditions: []snapshotv1.Condition{
						newProgressingCondition(corev1.ConditionFalse, "Waiting for target to be ready"),
						newReadyCondition(corev1.ConditionFalse, "Waiting for target to be ready"),
					},
				}
				addVolumeRestores(r)
				ur := r.DeepCopy()
				ur.ResourceVersion = "1"
				ur.Status.Conditions = []snapshotv1.Condition{
					newProgressingCondition(corev1.ConditionTrue, "Updating target spec"),
					newReadyCondition(corev1.ConditionFalse, "Waiting for target update"),
				}
				ur.Status.DeletedDataVolumes = getDeletedDataVolumes(createModifiedVM())
				for i := range ur.Status.Restores {
					ur.Status.Restores[i].DataVolumeName = &ur.Status.Restores[i].PersistentVolumeClaimName
				}

				vm := createModifiedVM()
				vmSource.Add(vm)
				expectUpdateVMRestoreInProgress(vm)
				vmRestoreSource.Add(r)
				expectPVCUpdates(k8sClient, ur)
				expectVMRestoreUpdate(kubevirtClient, ur)
				for _, pvc := range getRestorePVCs(r) {
					pvc.Status.Phase = corev1.ClaimBound
					addPVC(&pvc)
				}
				controller.processVMRestoreWorkItem()
			})

			It("should update VM spec", func() {
				r := createRestoreWithOwner()
				r.Status = &snapshotv1.VirtualMachineRestoreStatus{
					Complete:           &f,
					DeletedDataVolumes: getDeletedDataVolumes(createModifiedVM()),
					Conditions: []snapshotv1.Condition{
						newProgressingCondition(corev1.ConditionTrue, "Updating target spec"),
						newReadyCondition(corev1.ConditionFalse, "Waiting for target update"),
					},
				}
				addVolumeRestores(r)
				vm := createModifiedVM()
				vm.Status.RestoreInProgress = &vmRestoreName
				updatedVM := createSnapshotVM()
				updatedVM.Status.RestoreInProgress = &vmRestoreName
				updatedVM.ResourceVersion = "1"
				updatedVM.Annotations = map[string]string{"restore.kubevirt.io/lastRestoreUID": "restore-uid"}
				updatedVM.Spec.DataVolumeTemplates[0].Name = "restore-uid-disk1"
				updatedVM.Spec.Template.Spec.Volumes[0].DataVolume.Name = "restore-uid-disk1"
				for i := range r.Status.Restores {
					r.Status.Restores[i].DataVolumeName = &r.Status.Restores[i].PersistentVolumeClaimName
				}
				vmSource.Add(vm)
				vmInterface.EXPECT().Update(updatedVM).Return(updatedVM, nil)
				for _, pvc := range getRestorePVCs(r) {
					pvc.Annotations["cdi.kubevirt.io/storage.populatedFor"] = pvc.Name
					pvc.Status.Phase = corev1.ClaimBound
					pvcSource.Add(&pvc)
				}
				addVirtualMachineRestore(r)
				controller.processVMRestoreWorkItem()
			})

			It("should cleanup and complete", func() {
				r := createRestoreWithOwner()
				r.Status = &snapshotv1.VirtualMachineRestoreStatus{
					Complete:           &f,
					DeletedDataVolumes: getDeletedDataVolumes(createModifiedVM()),
					Conditions: []snapshotv1.Condition{
						newProgressingCondition(corev1.ConditionTrue, "Updating target spec"),
						newReadyCondition(corev1.ConditionFalse, "Waiting for target update"),
					},
				}
				addVolumeRestores(r)
				for i := range r.Status.Restores {
					r.Status.Restores[i].DataVolumeName = &r.Status.Restores[i].PersistentVolumeClaimName
				}

				vm := &v1.VirtualMachine{
					ObjectMeta: metav1.ObjectMeta{
						Name:      vmName,
						Namespace: testNamespace,
						UID:       vmUID,
						Annotations: map[string]string{
							"restore.kubevirt.io/lastRestoreUID": "restore-uid",
						},
					},
				}

				ur := r.DeepCopy()
				ur.ResourceVersion = "1"
				ur.Status.Complete = &t
				ur.Status.RestoreTime = timeFunc()
				ur.Status.Conditions = []snapshotv1.Condition{
					newProgressingCondition(corev1.ConditionFalse, "Operation complete"),
					newReadyCondition(corev1.ConditionTrue, "Operation complete"),
				}

				expectDataVolumeDeletes(cdiClient, r.Status.DeletedDataVolumes)
				expectVMRestoreUpdate(kubevirtClient, ur)

				for _, n := range r.Status.DeletedDataVolumes {
					dv := &cdiv1.DataVolume{
						ObjectMeta: metav1.ObjectMeta{
							Name:      n,
							Namespace: testNamespace,
						},
					}
					dataVolumeSource.Add(dv)
				}
				for _, pvc := range getRestorePVCs(r) {
					pvc.Annotations["cdi.kubevirt.io/storage.populatedFor"] = pvc.Name
					pvc.Status.Phase = corev1.ClaimBound
					pvcSource.Add(&pvc)
				}

				vmRestoreSource.Add(r)
				vm.Status.RestoreInProgress = &vmRestoreName
				addVM(vm)
				controller.processVMRestoreWorkItem()

				l, err := cdiClient.CdiV1beta1().DataVolumes("").List(context.Background(), metav1.ListOptions{})
				Expect(err).ToNot(HaveOccurred())
				Expect(l.Items).To(BeEmpty())
				testutils.ExpectEvent(recorder, "VirtualMachineRestoreComplete")
			})

			It("should unlock source VirtualMachine", func() {
				r := createRestoreWithOwner()
				r.Status = &snapshotv1.VirtualMachineRestoreStatus{
					Complete: &t,
				}
				vm := createModifiedVM()
				vm.Status.RestoreInProgress = &vmRestoreName
				vmSource.Add(vm)
				addVirtualMachineRestore(r)

				updatedVM := vm.DeepCopy()
				updatedVM.ResourceVersion = "1"
				updatedVM.Status.RestoreInProgress = nil
				vmInterface.EXPECT().UpdateStatus(updatedVM).Return(updatedVM, nil)
				controller.processVMRestoreWorkItem()
			})

			Context("target VM is different than source VM", func() {

				It("should be able to restore to a new VM", func() {
					By("Creating new VM")
					newVM := createVirtualMachine(testNamespace, "new-test-vm")
					newVM.Status.RestoreInProgress = &vmRestoreName
					newVM.UID = "new-vm-uid"
					vmSource.Add(newVM)

					By("Creating VM restore")
					vmRestore := createRestoreWithOwner()
					vmRestore.Spec.Target.Name = newVM.Name
					addVolumeRestores(vmRestore)
					addVirtualMachineRestore(vmRestore)

					By("Creating PVC")
					for _, pvc := range getRestorePVCs(vmRestore) {
						pvc.Status.Phase = corev1.ClaimBound
						addPVC(&pvc)
					}

					Expect(vmRestore.Status.Restores).To(HaveLen(1))
					vmRestore.Status.Restores[0].DataVolumeName = pointer.String(restoreDVName(vmRestore, vmRestore.Status.Restores[0].VolumeName))
					expectPVCUpdates(k8sClient, vmRestore)

					By("Making sure right VM update occurs")
					updatedVM := newVM.DeepCopy()
					updatedVM.Spec.DataVolumeTemplates[0].Name = *vmRestore.Status.Restores[0].DataVolumeName
					updatedVM.Spec.Template.Spec.Volumes[0].DataVolume.Name = *vmRestore.Status.Restores[0].DataVolumeName
					updatedVM.Annotations = map[string]string{lastRestoreAnnotation: "restore-uid"}
					updatedVM.ResourceVersion = "1"

					vmInterface.EXPECT().Update(updatedVM).Return(updatedVM, nil)

					By("Making sure right VMRestore update occurs")
					updatedVMRestore := vmRestore.DeepCopy()
					updatedVMRestore.Status.Conditions = []snapshotv1.Condition{
						newProgressingCondition(corev1.ConditionTrue, "Updating target spec"),
						newReadyCondition(corev1.ConditionFalse, "Waiting for target update"),
					}
					updatedVMRestore.ResourceVersion = "1"

					expectVMRestoreUpdate(kubevirtClient, updatedVMRestore)

					By("Running the controller")
					controller.processVMRestoreWorkItem()
				})

				It("should define owner reference properly", func() {
					By("Creating new VM")
					newVM := createVirtualMachine(testNamespace, "new-test-vm")
					newVM.Status.RestoreInProgress = &vmRestoreName
					newVM.UID = "new-vm-uid"
					vmSource.Add(newVM)

					By("Creating VM restore")
					vmRestore := createRestore()
					vmRestore.Spec.Target.Name = newVM.Name
					addVolumeRestores(vmRestore)
					addVirtualMachineRestore(vmRestore)

					By("Making sure right VMRestore update occurs")
					updatedVMRestore := vmRestore.DeepCopy()
					updatedVMRestore.Status.Conditions = []snapshotv1.Condition{
						newProgressingCondition(corev1.ConditionTrue, "Initializing VirtualMachineRestore"),
						newReadyCondition(corev1.ConditionFalse, "Initializing VirtualMachineRestore"),
					}
					updatedVMRestore.ResourceVersion = "1"
					updatedVMRestore.OwnerReferences = []metav1.OwnerReference{
						{
							APIVersion:         v1.GroupVersion.String(),
							Kind:               "VirtualMachine",
							Name:               newVM.Name,
							UID:                newVM.UID,
							Controller:         pointer.BoolPtr(true),
							BlockOwnerDeletion: pointer.BoolPtr(true),
						},
					}

					expectVMRestoreUpdate(kubevirtClient, updatedVMRestore)

					By("Running the controller")
					controller.processVMRestoreWorkItem()
				})

				Context("new VM does not exist, should create new VM", func() {

					const (
						newVmName     = "new-vm"
						newMacAddress = "00:00:5e:00:53:01"
					)

					var (
						r *snapshotv1.VirtualMachineRestore

						changeNamePatch       string
						changeMacAddressPatch string
					)

					BeforeEach(func() {
						r = createRestore()
						r.Spec.Target.Name = newVmName
						r.Status = &snapshotv1.VirtualMachineRestoreStatus{
							Complete: &f,
						}

						changeNamePatch = fmt.Sprintf(`{"op": "replace", "path": "/metadata/name", "value": "%s"}`, newVmName)
						changeMacAddressPatch = fmt.Sprintf(`{"op": "replace", "path": "/spec/template/spec/domain/devices/interfaces/0/macAddress", "value": "%s"}`, newMacAddress)

						err := vmSnapshotInformer.GetStore().Add(s)
						Expect(err).ShouldNot(HaveOccurred())

						err = vmSnapshotContentInformer.GetStore().Add(sc)
						Expect(err).ShouldNot(HaveOccurred())
					})

					It("with changed name", func() {
						r.Spec.Patches = []string{changeNamePatch}

						vmInterface.EXPECT().Create(gomock.Any()).DoAndReturn(func(newVM *v1.VirtualMachine) (*v1.VirtualMachine, error) {
							Expect(newVM.Name).To(Equal(newVmName), "the created VM should be the new VM")
							return newVM, nil
						}).Times(1)

						targetVM, err := controller.getTarget(r)
						Expect(err).ShouldNot(HaveOccurred())
						success, err := targetVM.Reconcile()
						Expect(success).To(BeTrue())
						Expect(err).ShouldNot(HaveOccurred())
					})

					It("with changed name and MAC address", func() {
						r.Spec.Patches = []string{changeNamePatch, changeMacAddressPatch}

						vmInterface.EXPECT().Create(gomock.Any()).DoAndReturn(func(newVM *v1.VirtualMachine) (*v1.VirtualMachine, error) {
							Expect(newVM.Name).To(Equal(newVmName), "the created VM should be the new VM")

							interfaces := newVM.Spec.Template.Spec.Domain.Devices.Interfaces
							Expect(interfaces).ToNot(BeEmpty())
							Expect(interfaces[0].MacAddress).To(Equal(newMacAddress))

							return newVM, nil
						}).Times(1)

						targetVM, err := controller.getTarget(r)
						Expect(err).ShouldNot(HaveOccurred())
						success, err := targetVM.Reconcile()
						Expect(success).To(BeTrue())
						Expect(err).ShouldNot(HaveOccurred())
					})

				})

			})
		})
	})
})

func expectPVCCreates(client *k8sfake.Clientset, vmRestore *snapshotv1.VirtualMachineRestore, expectedSize resource.Quantity) {
	client.Fake.PrependReactor("create", "persistentvolumeclaims", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
		create, ok := action.(testing.CreateAction)
		Expect(ok).To(BeTrue())

		createObj := create.GetObject().(*corev1.PersistentVolumeClaim)
		found := false
		for _, vr := range vmRestore.Status.Restores {
			if vr.PersistentVolumeClaimName == createObj.Name {
				Expect(createObj.Spec.Resources.Requests[corev1.ResourceStorage]).To(Equal(expectedSize))
				found = true
				break
			}
		}
		Expect(found).To(BeTrue())

		return true, create.GetObject(), nil
	})
}

func expectPVCUpdates(client *k8sfake.Clientset, vmRestore *snapshotv1.VirtualMachineRestore) {
	client.Fake.PrependReactor("update", "persistentvolumeclaims", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
		update, ok := action.(testing.UpdateAction)
		Expect(ok).To(BeTrue())

		updateObj := update.GetObject().(*corev1.PersistentVolumeClaim)
		found := false
		for _, vr := range vmRestore.Status.Restores {
			if vr.DataVolumeName != nil && *vr.DataVolumeName == updateObj.Annotations["cdi.kubevirt.io/storage.populatedFor"] {
				found = true
				break
			}
		}
		Expect(found).To(BeTrue())

		return true, update.GetObject(), nil
	})
}

func expectVMRestoreUpdate(client *kubevirtfake.Clientset, vmRestore *snapshotv1.VirtualMachineRestore) {
	client.Fake.PrependReactor("update", "virtualmachinerestores", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
		update, ok := action.(testing.UpdateAction)
		Expect(ok).To(BeTrue())

		updateObj := update.GetObject().(*snapshotv1.VirtualMachineRestore)
		Expect(updateObj).To(Equal(vmRestore))

		return true, update.GetObject(), nil
	})
}

func expectDataVolumeDeletes(client *cdifake.Clientset, names []string) {
	client.Fake.PrependReactor("delete", "datavolumes", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
		a, ok := action.(testing.DeleteAction)
		Expect(ok).To(BeTrue())

		dvName := a.GetName()
		found := false
		for _, name := range names {
			if name == dvName {
				found = true
				break
			}
		}
		Expect(found).To(BeTrue())

		return true, nil, nil
	})
}

// A mock to implement volumeSnapshotProvider interface
type MockVolumeSnapshotProvider struct {
	volumeSnapshots []*vsv1.VolumeSnapshot
}

func (v *MockVolumeSnapshotProvider) GetVolumeSnapshot(namespace, name string) (*vsv1.VolumeSnapshot, error) {
	if len(v.volumeSnapshots) == 0 {
		return nil, nil
	}
	vs := v.volumeSnapshots[0]
	v.volumeSnapshots = v.volumeSnapshots[1:]
	return vs, nil
}

func (v *MockVolumeSnapshotProvider) Add(s *vsv1.VolumeSnapshot) {
	v.volumeSnapshots = append(v.volumeSnapshots, s)
}
