// Copyright 2015 CNI authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"fmt"
	"net"
	"syscall"

	"github.com/containernetworking/cni/pkg/ns"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/testutils"
	"github.com/containernetworking/cni/pkg/types"

	"github.com/vishvananda/netlink"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("bridge Operations", func() {
	var originalNS ns.NetNS

	BeforeEach(func() {
		// Create a new NetNS so we don't modify the host
		var err error
		originalNS, err = ns.NewNS()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		Expect(originalNS.Close()).To(Succeed())
	})

	It("creates a bridge", func() {
		const IFNAME = "bridge0"

		conf := &NetConf{
			NetConf: types.NetConf{
				Name: "testConfig",
				Type: "bridge",
			},
			BrName: IFNAME,
			IsGW:   false,
			IPMasq: false,
			MTU:    5000,
		}

		err := originalNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()

			bridge, err := setupBridge(conf)
			Expect(err).NotTo(HaveOccurred())
			Expect(bridge.Attrs().Name).To(Equal(IFNAME))

			// Double check that the link was added
			link, err := netlink.LinkByName(IFNAME)
			Expect(err).NotTo(HaveOccurred())
			Expect(link.Attrs().Name).To(Equal(IFNAME))
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("handles an existing bridge", func() {
		const IFNAME = "bridge0"

		err := originalNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()

			err := netlink.LinkAdd(&netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name: IFNAME,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			link, err := netlink.LinkByName(IFNAME)
			Expect(err).NotTo(HaveOccurred())
			Expect(link.Attrs().Name).To(Equal(IFNAME))
			ifindex := link.Attrs().Index

			conf := &NetConf{
				NetConf: types.NetConf{
					Name: "testConfig",
					Type: "bridge",
				},
				BrName: IFNAME,
				IsGW:   false,
				IPMasq: false,
			}

			bridge, err := setupBridge(conf)
			Expect(err).NotTo(HaveOccurred())
			Expect(bridge.Attrs().Name).To(Equal(IFNAME))
			Expect(bridge.Attrs().Index).To(Equal(ifindex))

			// Double check that the link has the same ifindex
			link, err = netlink.LinkByName(IFNAME)
			Expect(err).NotTo(HaveOccurred())
			Expect(link.Attrs().Name).To(Equal(IFNAME))
			Expect(link.Attrs().Index).To(Equal(ifindex))
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("configures and deconfigures a bridge and veth with ADD/DEL", func() {
		const BRNAME = "cni0"
		const IFNAME = "eth0"

		gwaddr, subnet, err := net.ParseCIDR("10.1.2.1/24")
		Expect(err).NotTo(HaveOccurred())

		conf := fmt.Sprintf(`{
    "name": "mynet",
    "type": "bridge",
    "bridge": "%s",
    "isGateway": true,
    "ipMasq": false,
    "ipam": {
        "type": "host-local",
        "subnet": "%s"
    }
}`, BRNAME, subnet.String())

		targetNs, err := ns.NewNS()
		Expect(err).NotTo(HaveOccurred())
		defer targetNs.Close()

		args := &skel.CmdArgs{
			ContainerID: "dummy",
			Netns:       targetNs.Path(),
			IfName:      IFNAME,
			StdinData:   []byte(conf),
		}

		err = originalNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()

			_, err := testutils.CmdAddWithResult(targetNs.Path(), IFNAME, func() error {
				return cmdAdd(args)
			})
			Expect(err).NotTo(HaveOccurred())

			// Make sure bridge link exists
			link, err := netlink.LinkByName(BRNAME)
			Expect(err).NotTo(HaveOccurred())
			Expect(link.Attrs().Name).To(Equal(BRNAME))

			// Ensure bridge has gateway address
			addrs, err := netlink.AddrList(link, syscall.AF_INET)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(addrs)).To(BeNumerically(">", 0))
			found := false
			subnetPrefix, subnetBits := subnet.Mask.Size()
			for _, a := range addrs {
				aPrefix, aBits := a.IPNet.Mask.Size()
				if a.IPNet.IP.Equal(gwaddr) && aPrefix == subnetPrefix && aBits == subnetBits {
					found = true
					break
				}
			}
			Expect(found).To(Equal(true))

			// Check for the veth link in the main namespace
			links, err := netlink.LinkList()
			Expect(err).NotTo(HaveOccurred())
			Expect(len(links)).To(Equal(3)) // Bridge, veth, and loopback
			for _, l := range links {
				if l.Attrs().Name != BRNAME && l.Attrs().Name != "lo" {
					_, isVeth := l.(*netlink.Veth)
					Expect(isVeth).To(Equal(true))
				}
			}
			Expect(err).NotTo(HaveOccurred())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Find the veth peer in the container namespace
		err = targetNs.Do(func(ns.NetNS) error {
			defer GinkgoRecover()

			link, err := netlink.LinkByName(IFNAME)
			Expect(err).NotTo(HaveOccurred())
			Expect(link.Attrs().Name).To(Equal(IFNAME))
			return nil
		})
		Expect(err).NotTo(HaveOccurred())

		err = originalNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()

			err := testutils.CmdDelWithResult(targetNs.Path(), IFNAME, func() error {
				return cmdDel(args)
			})
			Expect(err).NotTo(HaveOccurred())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Make sure macvlan link has been deleted
		err = targetNs.Do(func(ns.NetNS) error {
			defer GinkgoRecover()

			link, err := netlink.LinkByName(IFNAME)
			Expect(err).To(HaveOccurred())
			Expect(link).To(BeNil())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
