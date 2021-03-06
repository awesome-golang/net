package main

import (
	"fmt"
	gonet "net"
	"strconv"

	"github.com/joshlf/net"
	"github.com/joshlf/net/internal/errors"
)

var udpIPv4Driver = deviceDriver{
	getDevice: func(args []string) (net.Device, error) {
		if len(args) != 4 {
			return nil, errors.Errorf("parse device definition: unexpected number of whitespace-separated fields: %v", len(args))
		}
		addr, subnet, err := net.ParseCIDRIPv4(args[0])
		if err != nil {
			return nil, errors.Annotate(err, "parse device definition")
		}
		laddr, err := gonet.ResolveUDPAddr("udp", args[1])
		if err != nil {
			return nil, errors.Annotate(err, "create device from definition")
		}
		raddr, err := gonet.ResolveUDPAddr("udp", args[2])
		if err != nil {
			return nil, errors.Annotate(err, "create device from definition")
		}
		mtu, err := strconv.Atoi(args[3])
		if err != nil {
			return nil, errors.Annotate(err, "parse device definition: parse MTU")
		}
		dev, err := net.NewUDPIPv4Device(laddr, raddr, mtu)
		if err != nil {
			return nil, errors.Annotate(err, "create device from definition")
		}
		err = dev.SetIPv4(addr, subnet.Netmask)
		return dev, errors.Annotate(err, "create device from definition")
	},
	getInfo: func(dev net.Device) (string, error) {
		udpdev := dev.(*net.UDPIPv4Device)
		laddr, raddr := udpdev.UDPAddrs()
		return fmt.Sprintf("%v -> %v", laddr, raddr), nil
	},
	init: func() {},
}

var udpIPv6Driver = deviceDriver{
	getDevice: func(args []string) (net.Device, error) {
		if len(args) != 4 {
			return nil, errors.Errorf("parse device definition: unexpected number of whitespace-separated fields: %v", len(args))
		}
		addr, subnet, err := net.ParseCIDRIPv6(args[0])
		if err != nil {
			return nil, errors.Annotate(err, "parse device definition")
		}
		laddr, err := gonet.ResolveUDPAddr("udp", args[1])
		if err != nil {
			return nil, errors.Annotate(err, "create device from definition")
		}
		raddr, err := gonet.ResolveUDPAddr("udp", args[2])
		if err != nil {
			return nil, errors.Annotate(err, "create device from definition")
		}
		mtu, err := strconv.Atoi(args[3])
		if err != nil {
			return nil, errors.Annotate(err, "parse device definition: parse MTU")
		}
		dev, err := net.NewUDPIPv6Device(laddr, raddr, mtu)
		if err != nil {
			return nil, errors.Annotate(err, "create device from definition")
		}
		err = dev.SetIPv6(addr, subnet.Netmask)
		return dev, errors.Annotate(err, "create device from definition")
	},
	getInfo: func(dev net.Device) (string, error) {
		udpdev := dev.(*net.UDPIPv6Device)
		laddr, raddr := udpdev.UDPAddrs()
		return fmt.Sprintf("%v -> %v", laddr, raddr), nil
	},
	init: func() {},
}

func init() {
	deviceDrivers["udp4"] = &udpIPv4Driver
	deviceDrivers["udp6"] = &udpIPv6Driver
}
