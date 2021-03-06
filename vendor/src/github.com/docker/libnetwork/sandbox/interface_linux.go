package sandbox

import (
	"fmt"
	"net"
	"sync"

	"github.com/docker/libnetwork/types"
	"github.com/vishvananda/netlink"
)

// IfaceOption is a function option type to set interface options
type IfaceOption func(i *nwIface)

type nwIface struct {
	srcName     string
	dstName     string
	master      string
	dstMaster   string
	address     *net.IPNet
	addressIPv6 *net.IPNet
	routes      []*net.IPNet
	bridge      bool
	ns          *networkNamespace
	sync.Mutex
}

func (i *nwIface) SrcName() string {
	i.Lock()
	defer i.Unlock()

	return i.srcName
}

func (i *nwIface) DstName() string {
	i.Lock()
	defer i.Unlock()

	return i.dstName
}

func (i *nwIface) DstMaster() string {
	i.Lock()
	defer i.Unlock()

	return i.dstMaster
}

func (i *nwIface) Bridge() bool {
	i.Lock()
	defer i.Unlock()

	return i.bridge
}

func (i *nwIface) Master() string {
	i.Lock()
	defer i.Unlock()

	return i.master
}

func (i *nwIface) Address() *net.IPNet {
	i.Lock()
	defer i.Unlock()

	return types.GetIPNetCopy(i.address)
}

func (i *nwIface) AddressIPv6() *net.IPNet {
	i.Lock()
	defer i.Unlock()

	return types.GetIPNetCopy(i.addressIPv6)
}

func (i *nwIface) Routes() []*net.IPNet {
	i.Lock()
	defer i.Unlock()

	routes := make([]*net.IPNet, len(i.routes))
	for index, route := range i.routes {
		r := types.GetIPNetCopy(route)
		routes[index] = r
	}

	return routes
}

func (n *networkNamespace) Interfaces() []Interface {
	n.Lock()
	defer n.Unlock()

	ifaces := make([]Interface, len(n.iFaces))

	for i, iface := range n.iFaces {
		ifaces[i] = iface
	}

	return ifaces
}

func (i *nwIface) Remove() error {
	i.Lock()
	n := i.ns
	i.Unlock()

	n.Lock()
	path := n.path
	n.Unlock()

	return nsInvoke(path, func(nsFD int) error { return nil }, func(callerFD int) error {
		// Find the network inteerface identified by the DstName attribute.
		iface, err := netlink.LinkByName(i.DstName())
		if err != nil {
			return err
		}

		// Down the interface before configuring
		if err := netlink.LinkSetDown(iface); err != nil {
			return err
		}

		err = netlink.LinkSetName(iface, i.SrcName())
		if err != nil {
			fmt.Println("LinkSetName failed: ", err)
			return err
		}

		// if it is a bridge just delete it.
		if i.Bridge() {
			if err := netlink.LinkDel(iface); err != nil {
				return fmt.Errorf("failed deleting bridge %q: %v", i.SrcName(), err)
			}
		} else {
			// Move the network interface to caller namespace.
			if err := netlink.LinkSetNsFd(iface, callerFD); err != nil {
				fmt.Println("LinkSetNsPid failed: ", err)
				return err
			}
		}

		n.Lock()
		for index, intf := range n.iFaces {
			if intf == i {
				n.iFaces = append(n.iFaces[:index], n.iFaces[index+1:]...)
				break
			}
		}
		n.Unlock()

		return nil
	})
}

func (n *networkNamespace) findDstMaster(srcName string) string {
	n.Lock()
	defer n.Unlock()

	for _, i := range n.iFaces {
		// The master should match the srcname of the interface and the
		// master interface should be of type bridge.
		if i.SrcName() == srcName && i.Bridge() {
			return i.DstName()
		}
	}

	return ""
}

func (n *networkNamespace) AddInterface(srcName, dstPrefix string, options ...IfaceOption) error {
	i := &nwIface{srcName: srcName, dstName: dstPrefix, ns: n}
	i.processInterfaceOptions(options...)

	if i.master != "" {
		i.dstMaster = n.findDstMaster(i.master)
		if i.dstMaster == "" {
			return fmt.Errorf("could not find an appropriate master %q for %q",
				i.master, i.srcName)
		}
	}

	n.Lock()
	i.dstName = fmt.Sprintf("%s%d", i.dstName, n.nextIfIndex)
	n.nextIfIndex++
	path := n.path
	n.Unlock()

	return nsInvoke(path, func(nsFD int) error {
		// If it is a bridge interface we have to create the bridge inside
		// the namespace so don't try to lookup the interface using srcName
		if i.bridge {
			return nil
		}

		// Find the network interface identified by the SrcName attribute.
		iface, err := netlink.LinkByName(i.srcName)
		if err != nil {
			return fmt.Errorf("failed to get link by name %q: %v", i.srcName, err)
		}

		// Move the network interface to the destination namespace.
		if err := netlink.LinkSetNsFd(iface, nsFD); err != nil {
			return fmt.Errorf("failed to set namespace on link %q: %v", i.srcName, err)
		}

		return nil
	}, func(callerFD int) error {
		if i.bridge {
			link := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name: i.srcName,
				},
			}

			if err := netlink.LinkAdd(link); err != nil {
				return fmt.Errorf("failed to create bridge %q: %v", i.srcName, err)
			}
		}

		// Find the network interface identified by the SrcName attribute.
		iface, err := netlink.LinkByName(i.srcName)
		if err != nil {
			return fmt.Errorf("failed to get link by name %q: %v", i.srcName, err)
		}

		// Down the interface before configuring
		if err := netlink.LinkSetDown(iface); err != nil {
			return fmt.Errorf("failed to set link down: %v", err)
		}

		// Configure the interface now this is moved in the proper namespace.
		if err := configureInterface(iface, i); err != nil {
			return err
		}

		// Up the interface.
		if err := netlink.LinkSetUp(iface); err != nil {
			return fmt.Errorf("failed to set link up: %v", err)
		}

		n.Lock()
		n.iFaces = append(n.iFaces, i)
		n.Unlock()

		return nil
	})
}

func configureInterface(iface netlink.Link, i *nwIface) error {
	ifaceName := iface.Attrs().Name
	ifaceConfigurators := []struct {
		Fn         func(netlink.Link, *nwIface) error
		ErrMessage string
	}{
		{setInterfaceName, fmt.Sprintf("error renaming interface %q to %q", ifaceName, i.DstName())},
		{setInterfaceIP, fmt.Sprintf("error setting interface %q IP to %q", ifaceName, i.Address())},
		{setInterfaceIPv6, fmt.Sprintf("error setting interface %q IPv6 to %q", ifaceName, i.AddressIPv6())},
		{setInterfaceRoutes, fmt.Sprintf("error setting interface %q routes to %q", ifaceName, i.Routes())},
		{setInterfaceMaster, fmt.Sprintf("error setting interface %q master to %q", ifaceName, i.DstMaster())},
	}

	for _, config := range ifaceConfigurators {
		if err := config.Fn(iface, i); err != nil {
			return fmt.Errorf("%s: %v", config.ErrMessage, err)
		}
	}
	return nil
}

func setInterfaceMaster(iface netlink.Link, i *nwIface) error {
	if i.DstMaster() == "" {
		return nil
	}

	return netlink.LinkSetMaster(iface, &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{Name: i.DstMaster()}})
}

func setInterfaceIP(iface netlink.Link, i *nwIface) error {
	if i.Address() == nil {
		return nil
	}

	ipAddr := &netlink.Addr{IPNet: i.Address(), Label: ""}
	return netlink.AddrAdd(iface, ipAddr)
}

func setInterfaceIPv6(iface netlink.Link, i *nwIface) error {
	if i.AddressIPv6() == nil {
		return nil
	}
	ipAddr := &netlink.Addr{IPNet: i.AddressIPv6(), Label: ""}
	return netlink.AddrAdd(iface, ipAddr)
}

func setInterfaceName(iface netlink.Link, i *nwIface) error {
	return netlink.LinkSetName(iface, i.DstName())
}

func setInterfaceRoutes(iface netlink.Link, i *nwIface) error {
	for _, route := range i.Routes() {
		err := netlink.RouteAdd(&netlink.Route{
			Scope:     netlink.SCOPE_LINK,
			LinkIndex: iface.Attrs().Index,
			Dst:       route,
		})
		if err != nil {
			return err
		}
	}
	return nil
}
