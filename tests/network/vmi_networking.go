/*
 * This file is part of the kubevirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2018 Red Hat, Inc.
 *
 */

package network

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"kubevirt.io/kubevirt/tests/framework/checks"

	expect "github.com/google/goexpect"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	k8sv1 "k8s.io/api/core/v1"
	v12 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v13 "k8s.io/apimachinery/pkg/apis/meta/v1"
	netutils "k8s.io/utils/net"
	"k8s.io/utils/pointer"

	"kubevirt.io/kubevirt/tests/util"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/client-go/log"
	"kubevirt.io/kubevirt/pkg/virt-controller/services"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
	"kubevirt.io/kubevirt/tests"
	"kubevirt.io/kubevirt/tests/console"
	"kubevirt.io/kubevirt/tests/flags"
	"kubevirt.io/kubevirt/tests/libnet"
	"kubevirt.io/kubevirt/tests/libvmi"
)

const (
	checkingEth0MACAddr = "checking eth0 MAC address"
	catResolvConf       = "cat /etc/resolv.conf\n"
)

var _ = SIGDescribe("[rfe_id:694][crit:medium][vendor:cnv-qe@redhat.com][level:component]Networking", func() {

	var err error
	var virtClient kubecli.KubevirtClient
	var currentConfiguration v1.KubeVirtConfiguration

	const (
		testPort                   = 1500
		LibvirtDirectMigrationPort = 49152
		LibvirtBlockMigrationPort  = 49153
	)

	BeforeEach(func() {
		tests.BeforeTestCleanup()

		virtClient, err = kubecli.GetKubevirtClient()
		util.PanicOnError(err)

		kv := util.GetCurrentKv(virtClient)
		currentConfiguration = kv.Spec.Configuration
	})

	checkMacAddress := func(vmi *v1.VirtualMachineInstance, expectedMacAddress string) {
		err := console.SafeExpectBatch(vmi, []expect.Batcher{
			&expect.BSnd{S: "\n"},
			&expect.BExp{R: console.PromptExpression},
			&expect.BSnd{S: "cat /sys/class/net/eth0/address\n"},
			&expect.BExp{R: expectedMacAddress},
		}, 15)
		Expect(err).ToNot(HaveOccurred())
	}

	checkNetworkVendor := func(vmi *v1.VirtualMachineInstance, expectedVendor string) {
		err := console.SafeExpectBatch(vmi, []expect.Batcher{
			&expect.BSnd{S: "\n"},
			&expect.BExp{R: console.PromptExpression},
			&expect.BSnd{S: "cat /sys/class/net/eth0/device/vendor\n"},
			&expect.BExp{R: expectedVendor},
		}, 15)
		Expect(err).ToNot(HaveOccurred())
	}

	checkLearningState := func(vmi *v1.VirtualMachineInstance, expectedValue string) {
		output := tests.RunCommandOnVmiPod(vmi, []string{"cat", "/sys/class/net/eth0-nic/brport/learning"})
		ExpectWithOffset(1, strings.TrimSpace(output)).To(Equal(expectedValue))
	}

	setBridgeEnabled := func(enable bool) {
		if currentConfiguration.NetworkConfiguration == nil {
			currentConfiguration.NetworkConfiguration = &v1.NetworkConfiguration{}
		}

		currentConfiguration.NetworkConfiguration.PermitBridgeInterfaceOnPodNetwork = pointer.BoolPtr(enable)
		kv := tests.UpdateKubeVirtConfigValueAndWait(currentConfiguration)
		currentConfiguration = kv.Spec.Configuration
	}

	Describe("Multiple virtual machines connectivity using bridge binding interface", func() {
		var inboundVMI *v1.VirtualMachineInstance
		var outboundVMI *v1.VirtualMachineInstance
		var inboundVMIWithPodNetworkSet *v1.VirtualMachineInstance
		var inboundVMIWithCustomMacAddress *v1.VirtualMachineInstance

		BeforeEach(func() {
			libnet.SkipWhenClusterNotSupportIpv4(virtClient)
		})
		Context("with a test outbound VMI", func() {
			BeforeEach(func() {
				inboundVMI = libvmi.NewCirros()
				outboundVMI = libvmi.NewCirros()
				inboundVMIWithPodNetworkSet = vmiWithPodNetworkSet()
				inboundVMIWithCustomMacAddress = vmiWithCustomMacAddress("de:ad:00:00:be:af")

				outboundVMI = runVMI(outboundVMI)
			})

			DescribeTable("should be able to reach", func(vmiRef **v1.VirtualMachineInstance) {
				var cmdCheck, addrShow, addr string
				if vmiRef == nil {
					addr = "kubevirt.io"
				} else {
					vmi := *vmiRef
					if vmiHasCustomMacAddress(vmi) {
						checks.SkipIfOpenShift("Custom MAC addresses on pod networks are not supported")
					}
					vmi = runVMI(vmi)
					addr = vmi.Status.Interfaces[0].IP
				}

				payloadSize := 0
				ipHeaderSize := 28 // IPv4 specific

				vmiPod := tests.GetRunningPodByVirtualMachineInstance(outboundVMI, util.NamespaceTestDefault)
				var mtu int
				for _, ifaceName := range []string{"k6t-eth0", "tap0"} {
					By(fmt.Sprintf("checking %s MTU inside the pod", ifaceName))
					output, err := tests.ExecuteCommandOnPod(
						virtClient,
						vmiPod,
						"compute",
						[]string{"cat", fmt.Sprintf("/sys/class/net/%s/mtu", ifaceName)},
					)
					log.Log.Infof("%s mtu is %v", ifaceName, output)
					Expect(err).ToNot(HaveOccurred())

					output = strings.TrimSuffix(output, "\n")
					mtu, err = strconv.Atoi(output)
					Expect(err).ToNot(HaveOccurred())

					Expect(mtu > 1000).To(BeTrue())

					payloadSize = mtu - ipHeaderSize
				}
				expectedMtuString := fmt.Sprintf("mtu %d", mtu)

				By("checking eth0 MTU inside the VirtualMachineInstance")
				Expect(libnet.WithIPv6(console.LoginToCirros)(outboundVMI)).To(Succeed())

				addrShow = "ip address show eth0\n"
				Expect(console.SafeExpectBatch(outboundVMI, []expect.Batcher{
					&expect.BSnd{S: "\n"},
					&expect.BExp{R: console.PromptExpression},
					&expect.BSnd{S: addrShow},
					&expect.BExp{R: fmt.Sprintf(".*%s.*\n", expectedMtuString)},
					&expect.BSnd{S: tests.EchoLastReturnValue},
					&expect.BExp{R: console.RetValue("0")},
				}, 180)).To(Succeed())

				By("checking the VirtualMachineInstance can send MTU sized frames to another VirtualMachineInstance")
				// NOTE: VirtualMachineInstance is not directly accessible from inside the pod because
				// we transferred its IP address under DHCP server control, so the
				// only thing we can validate is connectivity between VMIs
				//
				// NOTE: cirros ping doesn't support -M do that could be used to
				// validate end-to-end connectivity with Don't Fragment flag set
				cmdCheck = fmt.Sprintf("ping %s -c 1 -w 5 -s %d\n", addr, payloadSize)
				err = console.SafeExpectBatch(outboundVMI, []expect.Batcher{
					&expect.BSnd{S: "\n"},
					&expect.BExp{R: console.PromptExpression},
					&expect.BSnd{S: cmdCheck},
					&expect.BExp{R: console.PromptExpression},
					&expect.BSnd{S: tests.EchoLastReturnValue},
					&expect.BExp{R: console.RetValue("0")},
				}, 180)
				Expect(err).ToNot(HaveOccurred())

				By("checking the VirtualMachineInstance can fetch via HTTP")
				err = console.SafeExpectBatch(outboundVMI, []expect.Batcher{
					&expect.BSnd{S: "\n"},
					&expect.BExp{R: console.PromptExpression},
					&expect.BSnd{S: "curl --silent http://kubevirt.io > /dev/null\n"},
					&expect.BExp{R: console.PromptExpression},
					&expect.BSnd{S: tests.EchoLastReturnValue},
					&expect.BExp{R: console.RetValue("0")},
				}, 15)
				Expect(err).ToNot(HaveOccurred())
			},
				Entry("[test_id:1539]the Inbound VirtualMachineInstance", &inboundVMI),
				Entry("[test_id:1540]the Inbound VirtualMachineInstance with pod network connectivity explicitly set", &inboundVMIWithPodNetworkSet),
				Entry("[test_id:1541]the Inbound VirtualMachineInstance with custom MAC address", &inboundVMIWithCustomMacAddress),
				Entry("[test_id:1542]the internet", nil),
			)
		})

		Context("with propagated IP from a pod", func() {
			BeforeEach(func() {
				inboundVMI = libvmi.NewCirros()
				inboundVMI, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(inboundVMI)
				Expect(err).ToNot(HaveOccurred())
				inboundVMI = tests.WaitUntilVMIReady(inboundVMI, console.LoginToCirros)
				tests.StartTCPServer(inboundVMI, testPort)
			})

			DescribeTable("should be able to reach", func(op v12.NodeSelectorOperator, hostNetwork bool) {

				ip := inboundVMI.Status.Interfaces[0].IP

				//TODO if node count 1, skip the nv12.NodeSelectorOpOut
				nodes, err := virtClient.CoreV1().Nodes().List(context.Background(), v13.ListOptions{})
				Expect(err).ToNot(HaveOccurred())
				Expect(nodes.Items).ToNot(BeEmpty())
				if len(nodes.Items) == 1 && op == v12.NodeSelectorOpNotIn {
					Skip("Skip network test that requires multiple nodes when only one node is present.")
				}

				job := tests.NewHelloWorldJobTCP(ip, strconv.Itoa(testPort))
				job.Spec.Template.Spec.Affinity = &v12.Affinity{
					NodeAffinity: &v12.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &v12.NodeSelector{
							NodeSelectorTerms: []v12.NodeSelectorTerm{
								{
									MatchExpressions: []v12.NodeSelectorRequirement{
										{Key: "kubernetes.io/hostname", Operator: op, Values: []string{inboundVMI.Status.NodeName}},
									},
								},
							},
						},
					},
				}
				job.Spec.Template.Spec.HostNetwork = hostNetwork

				job, err = virtClient.BatchV1().Jobs(inboundVMI.ObjectMeta.Namespace).Create(context.Background(), job, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				Expect(tests.WaitForJobToSucceed(job, 90*time.Second)).To(Succeed())
			},
				Entry("[test_id:1543]on the same node from Pod", v12.NodeSelectorOpIn, false),
				Entry("[test_id:1544]on a different node from Pod", v12.NodeSelectorOpNotIn, false),
				Entry("[test_id:1545]on the same node from Node", v12.NodeSelectorOpIn, true),
				Entry("[test_id:1546]on a different node from Node", v12.NodeSelectorOpNotIn, true),
			)
		})

		Context("VirtualMachineInstance with default interface model", func() {
			BeforeEach(func() {
				inboundVMI = libvmi.NewCirros()
				outboundVMI = libvmi.NewCirros()

				inboundVMI, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(inboundVMI)
				Expect(err).ToNot(HaveOccurred())
				outboundVMI, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(outboundVMI)
				Expect(err).ToNot(HaveOccurred())

				inboundVMI = tests.WaitUntilVMIReady(inboundVMI, libnet.WithIPv6(console.LoginToCirros))
				outboundVMI = tests.WaitUntilVMIReady(outboundVMI, libnet.WithIPv6(console.LoginToCirros))
			})

			// Unless an explicit interface model is specified, the default interface model is virtio.
			It("[test_id:1550]should expose the right device type to the guest", func() {
				By("checking the device vendor in /sys/class")

				// Taken from https://wiki.osdev.org/Virtio#Technical_Details
				virtio_vid := "0x1af4"

				for _, networkVMI := range []*v1.VirtualMachineInstance{inboundVMI, outboundVMI} {
					// as defined in https://vendev.org/pci/ven_1af4/
					checkNetworkVendor(networkVMI, virtio_vid)
				}
			})

			Context("VirtualMachineInstance with unsupported interface model", func() {
				It("[test_id:1551]should reject the creation of virtual machine with unsupported interface model", func() {
					// Create a virtual machine with an unsupported interface model
					masqIface := libvmi.InterfaceDeviceWithMasqueradeBinding()
					masqIface.Model = "gibberish"
					customIfVMI := libvmi.NewAlpine(
						libvmi.WithInterface(masqIface),
						libvmi.WithNetwork(v1.DefaultPodNetwork()),
					)
					_, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(customIfVMI)
					Expect(err).To(HaveOccurred())
				})
			})
		})
	})

	Context("VirtualMachineInstance with custom interface model", func() {
		It("[test_id:1770]should expose the right device type to the guest", func() {
			By("checking the device vendor in /sys/class")
			// Create a machine with e1000 interface model
			// Use alpine because cirros dhcp client starts prematurely before link is ready
			masqIface := libvmi.InterfaceDeviceWithMasqueradeBinding()
			masqIface.Model = "e1000"
			e1000VMI := libvmi.NewAlpine(
				libvmi.WithInterface(masqIface),
				libvmi.WithNetwork(v1.DefaultPodNetwork()),
			)

			e1000VMI, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(e1000VMI)
			Expect(err).ToNot(HaveOccurred())

			tests.WaitUntilVMIReady(e1000VMI, console.LoginToAlpine)
			// as defined in https://vendev.org/pci/ven_8086/
			checkNetworkVendor(e1000VMI, "0x8086")
		})
	})

	Context("VirtualMachineInstance with custom MAC address", func() {
		It("[test_id:1771]should configure custom MAC address", func() {
			By(checkingEth0MACAddr)
			masqIface := libvmi.InterfaceDeviceWithMasqueradeBinding()
			masqIface.MacAddress = "de:ad:00:00:be:af"
			deadbeafVMI := libvmi.NewAlpine(
				libvmi.WithInterface(masqIface),
				libvmi.WithNetwork(v1.DefaultPodNetwork()),
			)
			deadbeafVMI, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(deadbeafVMI)
			Expect(err).ToNot(HaveOccurred())

			tests.WaitUntilVMIReady(deadbeafVMI, console.LoginToAlpine)
			checkMacAddress(deadbeafVMI, deadbeafVMI.Spec.Domain.Devices.Interfaces[0].MacAddress)
		})
	})

	Context("VirtualMachineInstance with custom MAC address in non-conventional format", func() {
		It("[test_id:1772]should configure custom MAC address", func() {
			By(checkingEth0MACAddr)
			masqIface := libvmi.InterfaceDeviceWithMasqueradeBinding()
			masqIface.MacAddress = "BE-AF-00-00-DE-AD"
			beafdeadVMI := libvmi.NewAlpine(
				libvmi.WithInterface(masqIface),
				libvmi.WithNetwork(v1.DefaultPodNetwork()),
			)
			beafdeadVMI, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(beafdeadVMI)
			Expect(err).ToNot(HaveOccurred())

			tests.WaitUntilVMIReady(beafdeadVMI, console.LoginToAlpine)
			checkMacAddress(beafdeadVMI, "be:af:00:00:de:ad")
		})
	})

	Context("VirtualMachineInstance with invalid MAC address", func() {
		It("[test_id:700]should failed to start with invalid MAC address", func() {
			By("Start VMI")
			masqIface := libvmi.InterfaceDeviceWithMasqueradeBinding()
			masqIface.MacAddress = "de:00c:00c:00:00:de:abc"
			beafdeadVMI := libvmi.NewAlpine(
				libvmi.WithInterface(masqIface),
				libvmi.WithNetwork(v1.DefaultPodNetwork()),
			)
			_, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(beafdeadVMI)
			Expect(err).To(HaveOccurred())
			testErr := err.(*errors.StatusError)
			Expect(testErr.ErrStatus.Reason).To(BeEquivalentTo("Invalid"))
		})
	})

	Context("VirtualMachineInstance with custom MAC address and slirp interface", func() {
		It("[test_id:1773]should configure custom MAC address", func() {
			By(checkingEth0MACAddr)
			slirpIface := libvmi.InterfaceDeviceWithSlirpBinding(libvmi.DefaultInterfaceName)
			slirpIface.MacAddress = "de:ad:00:00:be:af"
			deadbeafVMI := libvmi.NewAlpine(
				libvmi.WithInterface(slirpIface),
				libvmi.WithNetwork(v1.DefaultPodNetwork()),
			)
			deadbeafVMI, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(deadbeafVMI)
			Expect(err).ToNot(HaveOccurred())

			tests.WaitUntilVMIReady(deadbeafVMI, console.LoginToAlpine)
			checkMacAddress(deadbeafVMI, deadbeafVMI.Spec.Domain.Devices.Interfaces[0].MacAddress)
		})
	})

	Context("VirtualMachineInstance with disabled automatic attachment of interfaces", func() {
		It("[test_id:1774]should not configure any external interfaces", func() {
			By("checking loopback is the only guest interface")
			autoAttach := false
			detachedVMI := libvmi.NewCirros()
			// Remove the masquerade interface to use the default bridge one
			detachedVMI.Spec.Domain.Devices.Interfaces = nil
			detachedVMI.Spec.Networks = nil
			detachedVMI.Spec.Domain.Devices.AutoattachPodInterface = &autoAttach

			detachedVMI, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(detachedVMI)
			Expect(err).ToNot(HaveOccurred())
			tests.WaitUntilVMIReady(detachedVMI, libnet.WithIPv6(console.LoginToCirros))

			err := console.SafeExpectBatch(detachedVMI, []expect.Batcher{
				&expect.BSnd{S: "\n"},
				&expect.BExp{R: console.PromptExpression},
				&expect.BSnd{S: "ls /sys/class/net/ | wc -l\n"},
				&expect.BExp{R: "1"},
			}, 15)
			Expect(err).ToNot(HaveOccurred())
		})

		It("[test_id:1775]should not request a tun device", func() {
			By("Creating random VirtualMachineInstance")
			autoAttach := false
			vmi := libvmi.NewAlpine()
			vmi.Spec.Domain.Devices.AutoattachPodInterface = &autoAttach

			vmi, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(vmi)
			Expect(err).ToNot(HaveOccurred())
			waitUntilVMIReady(vmi, console.LoginToAlpine)

			By("Checking that the pod did not request a tun device")
			virtClient, err := kubecli.GetKubevirtClient()
			Expect(err).ToNot(HaveOccurred())

			By("Looking up pod using VMI's label")
			pods, err := virtClient.CoreV1().Pods(util.NamespaceTestDefault).List(context.Background(), tests.UnfinishedVMIPodSelector(vmi))
			Expect(err).ToNot(HaveOccurred())
			Expect(pods.Items).NotTo(BeEmpty())
			pod := pods.Items[0]

			foundContainer := false
			for _, container := range pod.Spec.Containers {
				if container.Name == "compute" {
					foundContainer = true
					_, ok := container.Resources.Requests[services.TunDevice]
					Expect(ok).To(BeFalse())

					_, ok = container.Resources.Limits[services.TunDevice]
					Expect(ok).To(BeFalse())

					caps := container.SecurityContext.Capabilities

					Expect(caps.Add).To(Not(ContainElement(k8sv1.Capability("NET_ADMIN"))), "Compute container should not have NET_ADMIN capability")
					Expect(caps.Drop).To(ContainElement(k8sv1.Capability("NET_RAW")), "Compute container should drop NET_RAW capability")
				}
			}

			Expect(foundContainer).To(BeTrue(), "Did not find 'compute' container in pod")
		})
	})

	Context("VirtualMachineInstance with custom PCI address", func() {
		checkPciAddress := func(vmi *v1.VirtualMachineInstance, expectedPciAddress string) {
			err := console.SafeExpectBatch(vmi, []expect.Batcher{
				&expect.BSnd{S: "\n"},
				&expect.BExp{R: console.PromptExpression},
				&expect.BSnd{S: "grep INTERFACE /sys/bus/pci/devices/" + expectedPciAddress + "/*/net/eth0/uevent|awk -F= '{ print $2 }'\n"},
				&expect.BExp{R: "eth0"},
			}, 15)
			Expect(err).ToNot(HaveOccurred())
		}

		It("[test_id:1776]should configure custom Pci address", func() {
			By("checking eth0 Pci address")
			testVMI := libvmi.NewCirros()
			tests.AddExplicitPodNetworkInterface(testVMI)
			testVMI.Spec.Domain.Devices.Interfaces[0].PciAddress = "0000:81:00.1"
			testVMI, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(testVMI)
			Expect(err).ToNot(HaveOccurred())

			tests.WaitUntilVMIReady(testVMI, libnet.WithIPv6(console.LoginToCirros))
			checkPciAddress(testVMI, testVMI.Spec.Domain.Devices.Interfaces[0].PciAddress)
		})
	})

	Context("VirtualMachineInstance with learning disabled on pod interface", func() {
		It("[test_id:1777]should disable learning on pod iface", func() {
			libnet.SkipWhenClusterNotSupportIpv4(virtClient)
			By("checking learning flag")
			learningDisabledVMI := libvmi.NewAlpine()
			learningDisabledVMI, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(learningDisabledVMI)
			Expect(err).ToNot(HaveOccurred())

			tests.WaitUntilVMIReady(learningDisabledVMI, console.LoginToAlpine)
			checkLearningState(learningDisabledVMI, "0")
		})
	})

	Context("VirtualMachineInstance with dhcp options", func() {
		It("[test_id:1778]should offer extra dhcp options to pod iface", func() {
			libnet.SkipWhenClusterNotSupportIpv4(virtClient)
			dhcpVMI := libvmi.NewTestToolingFedora()
			tests.AddExplicitPodNetworkInterface(dhcpVMI)

			dhcpVMI.Spec.Domain.Resources.Requests[k8sv1.ResourceName("memory")] = resource.MustParse("1024M")

			// This IPv4 address tests backwards compatibility of the "DHCPOptions.NTPServers" field.
			// The leading zero is intentional.
			// For more details please see: https://github.com/kubevirt/kubevirt/issues/6498
			const NTPServerWithLeadingZeros = "0127.0.0.3"

			dhcpVMI.Spec.Domain.Devices.Interfaces[0].DHCPOptions = &v1.DHCPOptions{
				BootFileName:   "config",
				TFTPServerName: "tftp.kubevirt.io",
				NTPServers:     []string{"127.0.0.1", "127.0.0.2", NTPServerWithLeadingZeros},
				PrivateOptions: []v1.DHCPPrivateOptions{{Option: 240, Value: "private.options.kubevirt.io"}},
			}

			dhcpVMI = tests.WaitUntilVMIReady(tests.RunVMI(dhcpVMI, 40), console.LoginToFedora)

			err = console.SafeExpectBatch(dhcpVMI, []expect.Batcher{
				&expect.BSnd{S: "\n"},
				&expect.BExp{R: console.PromptExpression},
				&expect.BSnd{S: "dhclient -1 -r -d eth0\n"},
				&expect.BExp{R: console.PromptExpression},
				&expect.BSnd{S: "dhclient -1 -sf /usr/bin/env --request-options subnet-mask,broadcast-address,time-offset,routers,domain-search,domain-name,domain-name-servers,host-name,nis-domain,nis-servers,ntp-servers,interface-mtu,tftp-server-name,bootfile-name eth0 | tee /dhcp-env\n"},
				&expect.BExp{R: console.PromptExpression},
				&expect.BSnd{S: "grep -q 'new_tftp_server_name=tftp.kubevirt.io' /dhcp-env; echo $?\n"},
				&expect.BExp{R: console.RetValue("0")},
				&expect.BSnd{S: "grep -q 'new_bootfile_name=config' /dhcp-env; echo $?\n"},
				&expect.BExp{R: console.RetValue("0")},
				&expect.BSnd{S: "grep -q 'new_ntp_servers=127.0.0.1 127.0.0.2 127.0.0.3' /dhcp-env; echo $?\n"},
				&expect.BExp{R: console.RetValue("0")},
				&expect.BSnd{S: "grep -q 'new_unknown_240=private.options.kubevirt.io' /dhcp-env; echo $?\n"},
				&expect.BExp{R: console.RetValue("0")},
			}, 15)

			Expect(err).ToNot(HaveOccurred())
		})
	})

	Context("VirtualMachineInstance with custom dns", func() {
		It("[test_id:1779]should have custom resolv.conf", func() {
			libnet.SkipWhenClusterNotSupportIpv4(virtClient)
			userData := "#cloud-config\n"
			dnsVMI := libvmi.NewCirros(libvmi.WithCloudInitNoCloudUserData(userData, false))

			dnsVMI.Spec.DNSPolicy = "None"

			// This IPv4 address tests backwards compatibility of the "DNSConfig.Nameservers" field.
			// The leading zero is intentional.
			// For more details please see: https://github.com/kubevirt/kubevirt/issues/6498
			const DNSNameserverWithLeadingZeros = "01.1.1.1"
			dnsVMI.Spec.DNSConfig = &k8sv1.PodDNSConfig{
				Nameservers: []string{"8.8.8.8", "4.2.2.1", DNSNameserverWithLeadingZeros},
				Searches:    []string{"example.com"},
			}
			dnsVMI = tests.WaitUntilVMIReady(tests.RunVMI(dnsVMI, 40), libnet.WithIPv6(console.LoginToCirros))
			err = console.SafeExpectBatch(dnsVMI, []expect.Batcher{
				&expect.BSnd{S: "\n"},
				&expect.BExp{R: console.PromptExpression},
				&expect.BSnd{S: catResolvConf},
				&expect.BExp{R: "search example.com"},
				&expect.BSnd{S: "\n"},
				&expect.BExp{R: console.PromptExpression},
				&expect.BSnd{S: catResolvConf},
				&expect.BExp{R: "nameserver 8.8.8.8"},
				&expect.BSnd{S: "\n"},
				&expect.BExp{R: console.PromptExpression},
				&expect.BSnd{S: catResolvConf},
				&expect.BExp{R: "nameserver 4.2.2.1"},
				&expect.BSnd{S: "\n"},
				&expect.BExp{R: console.PromptExpression},
				&expect.BSnd{S: "cat /etc/resolv.conf\n"},
				&expect.BExp{R: "nameserver 1.1.1.1"},
				&expect.BSnd{S: "\n"},
				&expect.BExp{R: console.PromptExpression},
			}, 15)
			Expect(err).ToNot(HaveOccurred())
		})
	})

	Context("VirtualMachineInstance with masquerade binding mechanism", func() {
		masqueradeVMI := func(ports []v1.Port, ipv4NetworkCIDR string) *v1.VirtualMachineInstance {
			net := v1.DefaultPodNetwork()
			if ipv4NetworkCIDR != "" {
				net.NetworkSource.Pod.VMNetworkCIDR = ipv4NetworkCIDR
			}
			return libvmi.NewCirros(
				libvmi.WithInterface(libvmi.InterfaceDeviceWithMasqueradeBinding(ports...)),
				libvmi.WithNetwork(net),
			)
		}

		fedoraMasqueradeVMI := func(ports []v1.Port, ipv6NetworkCIDR string) (*v1.VirtualMachineInstance, error) {

			networkData, err := libnet.NewNetworkData(
				libnet.WithEthernet("eth0",
					libnet.WithDHCP4Enabled(),
					libnet.WithDHCP6Enabled(),
				),
			)
			if err != nil {
				return nil, err
			}

			net := v1.DefaultPodNetwork()
			net.Pod.VMIPv6NetworkCIDR = ipv6NetworkCIDR
			vmi := libvmi.NewFedora(
				libvmi.WithInterface(libvmi.InterfaceDeviceWithMasqueradeBinding(ports...)),
				libvmi.WithNetwork(net),
				libvmi.WithCloudInitNoCloudNetworkData(networkData, false),
			)

			return vmi, nil
		}

		configureIpv6 := func(vmi *v1.VirtualMachineInstance, networkCIDR string) error {
			if networkCIDR == "" {
				networkCIDR = api.DefaultVMIpv6CIDR
			}

			err := console.RunCommand(vmi, "dhclient -6 eth0", 30*time.Second)
			if err != nil {
				return err
			}
			err = console.RunCommand(vmi, "ip -6 route add "+networkCIDR+" dev eth0", 5*time.Second)
			if err != nil {
				return err
			}
			gateway := gatewayIPFromCIDR(networkCIDR)
			err = console.RunCommand(vmi, "ip -6 route add default via "+gateway, 5*time.Second)
			if err != nil {
				return err
			}
			return nil
		}

		portsUsedByLiveMigration := func() []v1.Port {
			return []v1.Port{
				{Port: LibvirtDirectMigrationPort},
				{Port: LibvirtBlockMigrationPort},
			}
		}

		Context("[Conformance][test_id:1780][label:masquerade_binding_connectivity]should allow regular network connection", func() {
			// This CIDR tests backwards compatibility of the "vmNetworkCIDR" field.
			// The leading zero is intentional.
			// For more details please see: https://github.com/kubevirt/kubevirt/issues/6498
			const cidrWithLeadingZeros = "10.10.010.0/24"

			verifyClientServerConnectivity := func(clientVMI *v1.VirtualMachineInstance, serverVMI *v1.VirtualMachineInstance, tcpPort int, ipFamily k8sv1.IPFamily) error {
				serverIP := libnet.GetVmiPrimaryIpByFamily(serverVMI, ipFamily)
				err := libnet.PingFromVMConsole(clientVMI, serverIP)
				if err != nil {
					return err
				}

				By("Connecting from the client vm")
				err = console.SafeExpectBatch(clientVMI, createExpectConnectToServer(serverIP, tcpPort, true), 30)
				if err != nil {
					return err
				}

				By("Rejecting the connection from the client to unregistered port")
				err = console.SafeExpectBatch(clientVMI, createExpectConnectToServer(serverIP, tcpPort+1, false), 30)
				if err != nil {
					return err
				}

				return nil
			}

			DescribeTable("ipv4", func(ports []v1.Port, tcpPort int, networkCIDR string) {
				libnet.SkipWhenClusterNotSupportIpv4(virtClient)

				var clientVMI *v1.VirtualMachineInstance
				var serverVMI *v1.VirtualMachineInstance

				clientVMI = masqueradeVMI([]v1.Port{}, networkCIDR)
				clientVMI, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(clientVMI)
				Expect(err).ToNot(HaveOccurred())
				clientVMI = tests.WaitUntilVMIReady(clientVMI, console.LoginToCirros)

				serverVMI = masqueradeVMI(ports, networkCIDR)

				serverVMI.Labels = map[string]string{"expose": "server"}
				serverVMI, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(serverVMI)
				Expect(err).ToNot(HaveOccurred())
				serverVMI = tests.WaitUntilVMIReady(serverVMI, console.LoginToCirros)
				Expect(serverVMI.Status.Interfaces).To(HaveLen(1))
				Expect(serverVMI.Status.Interfaces[0].IPs).NotTo(BeEmpty())

				By("starting a tcp server")
				tests.StartTCPServer(serverVMI, tcpPort)

				if networkCIDR == "" {
					networkCIDR = api.DefaultVMCIDR
				}

				By("Checking ping (IPv4) to gateway")
				ipAddr := gatewayIPFromCIDR(networkCIDR)
				Expect(libnet.PingFromVMConsole(serverVMI, ipAddr)).To(Succeed())

				Expect(verifyClientServerConnectivity(clientVMI, serverVMI, tcpPort, k8sv1.IPv4Protocol)).To(Succeed())
			},
				Entry("with a specific port number [IPv4]", []v1.Port{{Name: "http", Port: 8080}}, 8080, ""),
				Entry("with a specific port used by live migration", portsUsedByLiveMigration(), LibvirtDirectMigrationPort, ""),
				Entry("without a specific port number [IPv4]", []v1.Port{}, 8080, ""),
				Entry("with custom CIDR [IPv4]", []v1.Port{}, 8080, "10.10.10.0/24"),
				Entry("with custom CIDR [IPv4] containing leading zeros", []v1.Port{}, 8080, cidrWithLeadingZeros),
			)

			It("[outside_connectivity]should be able to reach the outside world [IPv4]", func() {
				libnet.SkipWhenClusterNotSupportIpv4(virtClient)
				ipv4Address := "8.8.8.8"
				if flags.IPV4ConnectivityCheckAddress != "" {
					ipv4Address = flags.IPV4ConnectivityCheckAddress
				}
				dns := "google.com"
				if flags.ConnectivityCheckDNS != "" {
					dns = flags.ConnectivityCheckDNS
				}

				vmi := masqueradeVMI([]v1.Port{}, "")
				vmi, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(vmi)
				Expect(err).ToNot(HaveOccurred())
				vmi = tests.WaitUntilVMIReady(vmi, console.LoginToCirros)

				By("Checking ping (IPv4)")
				Expect(libnet.PingFromVMConsole(vmi, ipv4Address, "-c 5", "-w 15")).To(Succeed())
				Expect(libnet.PingFromVMConsole(vmi, dns, "-c 5", "-w 15")).To(Succeed())
			})

			DescribeTable("IPv6", func(ports []v1.Port, tcpPort int, networkCIDR string) {
				libnet.SkipWhenClusterNotSupportIpv6(virtClient)
				var serverVMI *v1.VirtualMachineInstance
				var clientVMI *v1.VirtualMachineInstance

				clientVMI, err = fedoraMasqueradeVMI([]v1.Port{}, networkCIDR)
				Expect(err).ToNot(HaveOccurred())
				clientVMI, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(clientVMI)
				Expect(err).ToNot(HaveOccurred())
				clientVMI = tests.WaitUntilVMIReady(clientVMI, console.LoginToFedora)

				Expect(configureIpv6(clientVMI, networkCIDR)).To(Succeed(), "failed to configure ipv6 on client vmi")

				serverVMI, err = fedoraMasqueradeVMI(ports, networkCIDR)
				Expect(err).ToNot(HaveOccurred())

				serverVMI.Labels = map[string]string{"expose": "server"}
				serverVMI, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(serverVMI)
				Expect(err).ToNot(HaveOccurred())
				serverVMI = tests.WaitUntilVMIReady(serverVMI, console.LoginToFedora)

				Expect(configureIpv6(serverVMI, networkCIDR)).To(Succeed(), "failed to configure ipv6  on server vmi")

				Expect(serverVMI.Status.Interfaces).To(HaveLen(1))
				Expect(serverVMI.Status.Interfaces[0].IPs).NotTo(BeEmpty())

				By("starting a http server")
				tests.StartPythonHttpServer(serverVMI, tcpPort)

				Expect(verifyClientServerConnectivity(clientVMI, serverVMI, tcpPort, k8sv1.IPv6Protocol)).To(Succeed())
			},
				Entry("with a specific port number [IPv6]", []v1.Port{{Name: "http", Port: 8080}}, 8080, ""),
				Entry("with a specific port used by live migration", portsUsedByLiveMigration(), LibvirtDirectMigrationPort, ""),
				Entry("without a specific port number [IPv6]", []v1.Port{}, 8080, ""),
				Entry("with custom CIDR [IPv6]", []v1.Port{}, 8080, "fd10:10:10::/120"),
			)

			It("[outside_connectivity]should be able to reach the outside world [IPv6]", func() {
				libnet.SkipWhenClusterNotSupportIpv6(virtClient)
				// Cluster nodes subnet (docker network gateway)
				// Docker network subnet cidr definition:
				// https://github.com/kubevirt/project-infra/blob/master/github/ci/shared-deployments/files/docker-daemon-mirror.conf#L5
				ipv6Address := "2001:db8:1::1"
				if flags.IPV6ConnectivityCheckAddress != "" {
					ipv6Address = flags.IPV6ConnectivityCheckAddress
				}

				vmi, err := fedoraMasqueradeVMI([]v1.Port{}, "")
				Expect(err).ToNot(HaveOccurred())
				vmi, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(vmi)
				Expect(err).ToNot(HaveOccurred())
				vmi = tests.WaitUntilVMIReady(vmi, console.LoginToFedora)
				Expect(configureIpv6(vmi, api.DefaultVMIpv6CIDR)).To(Succeed(), "failed to configure ipv6 on vmi")

				By("Checking ping (IPv6) from vmi to cluster nodes gateway")
				Expect(libnet.PingFromVMConsole(vmi, ipv6Address)).To(Succeed())
			})
		})

		When("performing migration", func() {
			var vmi *v1.VirtualMachineInstance

			ping := func(ipAddr string) error {
				return libnet.PingFromVMConsole(vmi, ipAddr, "-c 1", "-w 2")
			}

			getVirtHandlerPod := func() (*k8sv1.Pod, error) {
				node := vmi.Status.NodeName
				pod, err := kubecli.NewVirtHandlerClient(virtClient).Namespace(flags.KubeVirtInstallNamespace).ForNode(node).Pod()
				if err != nil {
					return nil, fmt.Errorf("failed to get virt-handler pod on node %s: %v", node, err)
				}
				return pod, nil
			}

			runMigrationAndExpectCompletion := func(migration *v1.VirtualMachineInstanceMigration, timeout int) {
				migration, err = virtClient.VirtualMachineInstanceMigration(migration.Namespace).Create(migration, &metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				Eventually(func() error {
					migration, err := virtClient.VirtualMachineInstanceMigration(migration.Namespace).Get(migration.Name, &v13.GetOptions{})
					if err != nil {
						return err
					}

					Expect(migration.Status.Phase).ToNot(Equal(v1.MigrationFailed))

					if migration.Status.Phase == v1.MigrationSucceeded {
						return nil
					}
					return fmt.Errorf("Migration is in phase %s", migration.Status.Phase)

				}, timeout, time.Second).Should(Succeed(), fmt.Sprintf("migration should succeed after %d s", timeout))
			}

			BeforeEach(func() {
				checks.SkipIfMigrationIsNotPossible()
			})

			AfterEach(func() {
				if vmi != nil {
					By("Delete VMI")
					Expect(virtClient.VirtualMachineInstance(vmi.Namespace).Delete(vmi.Name, &v13.DeleteOptions{})).To(Succeed())

					Eventually(func() error {
						_, err := virtClient.VirtualMachineInstance(vmi.Namespace).Get(vmi.Name, &v13.GetOptions{})
						return err
					}, time.Minute, time.Second).Should(
						SatisfyAll(HaveOccurred(), WithTransform(errors.IsNotFound, BeTrue())),
						"The VMI should be gone within the given timeout",
					)
				}
			})

			DescribeTable("[Conformance] preserves connectivity", func(ipFamily k8sv1.IPFamily, ports []v1.Port) {
				libnet.SkipWhenClusterNotSupportIpFamily(virtClient, ipFamily)

				var err error
				var loginMethod console.LoginToFunction

				By("Create VMI")
				if ipFamily == k8sv1.IPv4Protocol {
					vmi = masqueradeVMI(ports, "")
					loginMethod = console.LoginToCirros
				} else {
					vmi, err = fedoraMasqueradeVMI(ports, "")
					Expect(err).ToNot(HaveOccurred(), "Error creating fedora masquerade vmi")
					loginMethod = console.LoginToFedora
				}

				vmi, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(vmi)
				Expect(err).ToNot(HaveOccurred())
				vmi = tests.WaitUntilVMIReady(vmi, loginMethod)

				if ipFamily == k8sv1.IPv6Protocol {
					err = configureIpv6(vmi, api.DefaultVMIpv6CIDR)
					Expect(err).ToNot(HaveOccurred(), "failed to configure ipv6 on vmi")
				}

				virtHandlerPod, err := getVirtHandlerPod()
				Expect(err).ToNot(HaveOccurred())

				By("Check connectivity")
				podIP := libnet.GetPodIpByFamily(virtHandlerPod, ipFamily)
				Expect(ping(podIP)).To(Succeed())

				By("Execute migration")
				migration := tests.NewRandomMigration(vmi.Name, vmi.Namespace)
				runMigrationAndExpectCompletion(migration, tests.MigrationWaitTime)

				vmi, err = virtClient.VirtualMachineInstance(vmi.Namespace).Get(vmi.Name, &v13.GetOptions{})
				Expect(err).ToNot(HaveOccurred())
				Expect(vmi.Status.Phase).To(Equal(v1.Running))

				Expect(ping(podIP)).To(Succeed())

				By("Restarting the vmi")
				Expect(console.SafeExpectBatch(vmi, []expect.Batcher{
					&expect.BSnd{S: "sudo reboot\n"},
					&expect.BExp{R: "reboot: Restarting system"},
				}, 10)).To(Succeed(), "failed to restart the vmi")
				tests.WaitUntilVMIReady(vmi, loginMethod)
				if ipFamily == k8sv1.IPv6Protocol {
					Expect(configureIpv6(vmi, api.DefaultVMIpv6CIDR)).To(Succeed(), "failed to configure ipv6 on vmi after restart")
				}
				Expect(ping(podIP)).To(Succeed())
			},
				Entry("IPv4", k8sv1.IPv4Protocol, []v1.Port{}),
				Entry("IPv4 with explicit ports used by live migration", k8sv1.IPv4Protocol, portsUsedByLiveMigration()),
				Entry("IPv6", k8sv1.IPv6Protocol, []v1.Port{}),
			)
		})

		Context("MTU verification", func() {
			var vmi *v1.VirtualMachineInstance
			var anotherVmi *v1.VirtualMachineInstance

			getMtu := func(pod *k8sv1.Pod, ifaceName string) int {
				output, err := tests.ExecuteCommandOnPod(
					virtClient,
					pod,
					"compute",
					[]string{"cat", fmt.Sprintf("/sys/class/net/%s/mtu", ifaceName)},
				)
				ExpectWithOffset(1, err).ToNot(HaveOccurred())

				output = strings.TrimSuffix(output, "\n")
				mtu, err := strconv.Atoi(output)
				ExpectWithOffset(1, err).ToNot(HaveOccurred())
				return mtu
			}

			BeforeEach(func() {
				var err error

				By("Create masquerade VMI")
				networkData, err := libnet.CreateDefaultCloudInitNetworkData()
				Expect(err).NotTo(HaveOccurred())

				vmi = libvmi.NewFedora(
					libvmi.WithInterface(libvmi.InterfaceDeviceWithMasqueradeBinding()),
					libvmi.WithNetwork(v1.DefaultPodNetwork()),
					libvmi.WithCloudInitNoCloudNetworkData(networkData, false),
				)

				vmi, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(vmi)
				Expect(err).ToNot(HaveOccurred())

				By("Create another VMI")
				anotherVmi = masqueradeVMI([]v1.Port{}, "")
				anotherVmi, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(anotherVmi)
				Expect(err).ToNot(HaveOccurred())

				By("Wait for VMIs to be ready")
				anotherVmi = tests.WaitUntilVMIReady(anotherVmi, libnet.WithIPv6(console.LoginToCirros))

				vmi = tests.WaitUntilVMIReady(vmi, console.LoginToFedora)
			})

			DescribeTable("should have the correct MTU", func(ipFamily k8sv1.IPFamily) {
				libnet.SkipWhenClusterNotSupportIpFamily(virtClient, ipFamily)

				By("checking k6t-eth0 MTU inside the pod")
				vmiPod := tests.GetRunningPodByVirtualMachineInstance(vmi, vmi.Namespace)
				bridgeMtu := getMtu(vmiPod, "k6t-eth0")
				primaryIfaceMtu := getMtu(vmiPod, "eth0")

				Expect(bridgeMtu).To(Equal(primaryIfaceMtu), "k6t-eth0 bridge mtu should equal eth0 interface mtu")

				By("checking the tap device - tap0 - MTU inside the pod")
				tapDeviceMTU := getMtu(vmiPod, "tap0")
				Expect(tapDeviceMTU).To(Equal(primaryIfaceMtu), "tap0 mtu should equal eth0 interface mtu")

				By("checking eth0 MTU inside the VirtualMachineInstance")
				showMtu := "cat /sys/class/net/eth0/mtu\n"
				err = console.SafeExpectBatch(vmi, []expect.Batcher{
					&expect.BSnd{S: showMtu},
					&expect.BExp{R: console.RetValue(strconv.Itoa(bridgeMtu))},
				}, 180)
				Expect(err).ToNot(HaveOccurred())

				By("checking the VirtualMachineInstance can send MTU sized frames to another VirtualMachineInstance")
				icmpHeaderSize := 8
				var ipHeaderSize int
				if ipFamily == k8sv1.IPv4Protocol {
					ipHeaderSize = 20
				} else {
					ipHeaderSize = 40
				}
				payloadSize := primaryIfaceMtu - ipHeaderSize - icmpHeaderSize
				addr := libnet.GetVmiPrimaryIpByFamily(anotherVmi, ipFamily)
				Expect(libnet.PingFromVMConsole(vmi, addr, "-c 1", "-w 5", fmt.Sprintf("-s %d", payloadSize), "-M do")).To(Succeed())

				By("checking the VirtualMachineInstance cannot send bigger than MTU sized frames to another VirtualMachineInstance")
				Expect(libnet.PingFromVMConsole(vmi, addr, "-c 1", "-w 5", fmt.Sprintf("-s %d", payloadSize+1), "-M do")).ToNot(Succeed())
			},
				Entry("IPv4", k8sv1.IPv4Protocol),
				Entry("IPv6", k8sv1.IPv6Protocol),
			)
		})
	})

	Context("VirtualMachineInstance with TX offload disabled", func() {
		It("[test_id:1781]should have tx checksumming disabled on interface serving dhcp", func() {
			vmi := libvmi.NewAlpine()
			vmi.Spec.Domain.Resources.Requests[k8sv1.ResourceName("memory")] = resource.MustParse("1024M")

			vmi, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(vmi)
			Expect(err).ToNot(HaveOccurred())
			tests.WaitUntilVMIReady(vmi, console.LoginToAlpine)
			output := tests.RunCommandOnVmiPod(
				vmi,
				[]string{"/bin/bash", "-c", "/usr/sbin/ethtool -k k6t-eth0|grep tx-checksumming|awk '{ printf $2 }'"},
			)
			ExpectWithOffset(1, strings.TrimSpace(output)).To(Equal("off"))
		})
	})

	Context("[Serial]vmi with default bridge interface on pod network", func() {
		BeforeEach(func() {
			setBridgeEnabled(false)
		})
		AfterEach(func() {
			setBridgeEnabled(true)
		})
		It("[test_id:2964]should reject VMIs with bridge interface when it's not permitted on pod network", func() {
			vmi := libvmi.NewCirros()

			_, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(vmi)
			Expect(err.Error()).To(ContainSubstring("Bridge interface is not enabled in kubevirt-config"))
		})
	})
})

func waitUntilVMIReady(vmi *v1.VirtualMachineInstance, loginTo console.LoginToFunction) *v1.VirtualMachineInstance {
	vmi = tests.WaitForSuccessfulVMIStart(vmi)

	// make sure that the OS is up by waiting until we can login
	Expect(loginTo(vmi)).To(Succeed())
	return vmi
}

func createExpectConnectToServer(serverIP string, tcpPort int, expectSuccess bool) []expect.Batcher {
	expectResult := console.ShellFail
	if expectSuccess {
		expectResult = console.ShellSuccess
	}

	var clientCommand string

	if netutils.IsIPv6String(serverIP) {
		clientCommand = fmt.Sprintf("curl %s\n", net.JoinHostPort(serverIP, strconv.Itoa(tcpPort)))
	} else {
		clientCommand = fmt.Sprintf("echo test | nc %s %d -i 1 -w 1 1> /dev/null\n", serverIP, tcpPort)
	}
	return []expect.Batcher{
		&expect.BSnd{S: "\n"},
		&expect.BExp{R: console.PromptExpression},
		&expect.BSnd{S: clientCommand},
		&expect.BExp{R: console.PromptExpression},
		&expect.BSnd{S: tests.EchoLastReturnValue},
		&expect.BExp{R: expectResult},
	}
}

// gatewayIpFromCIDR returns the first address of a network.
func gatewayIPFromCIDR(cidr string) string {
	// ParseCIDRSloppy is intentionally used to test backwards compatibility of the "vmNetworkCIDR" field with leading zeros.
	// For more details please see: https://github.com/kubevirt/kubevirt/issues/6498
	ip, ipnet, _ := netutils.ParseCIDRSloppy(cidr)
	ip = ip.Mask(ipnet.Mask)
	oct := len(ip) - 1
	ip[oct]++
	return ip.String()
}

func vmiHasCustomMacAddress(vmi *v1.VirtualMachineInstance) bool {
	return vmi.Spec.Domain.Devices.Interfaces != nil &&
		vmi.Spec.Domain.Devices.Interfaces[0].MacAddress != ""
}

func runVMI(vmi *v1.VirtualMachineInstance) *v1.VirtualMachineInstance {
	virtClient, err := kubecli.GetKubevirtClient()
	util.PanicOnError(err)

	vmi, err = virtClient.VirtualMachineInstance(util.NamespaceTestDefault).Create(vmi)
	Expect(err).ToNot(HaveOccurred())
	vmi = tests.WaitUntilVMIReady(vmi, libnet.WithIPv6(console.LoginToCirros))
	return vmi
}

func vmiWithPodNetworkSet() *v1.VirtualMachineInstance {
	return libvmi.NewCirros(
		libvmi.WithInterface(*v1.DefaultBridgeNetworkInterface()),
		libvmi.WithNetwork(v1.DefaultPodNetwork()))
}

func vmiWithCustomMacAddress(mac string) *v1.VirtualMachineInstance {
	return libvmi.NewCirros(
		libvmi.WithInterface(*libvmi.InterfaceWithMac(v1.DefaultBridgeNetworkInterface(), mac)),
		libvmi.WithNetwork(v1.DefaultPodNetwork()))
}
