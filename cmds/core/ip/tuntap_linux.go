// Copyright 2024 the u-root Authors. All rights reserved
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"math"

	"github.com/vishvananda/netlink"
)

const (
	tuntapHelp = `Usage: ip tuntap { add | del | show | list | lst | help } [ dev | name ] NAME ]
       [ mode { tun | tap } ] [ user USER ] [ group GROUP ]
       [ one_queue ] [ pi ] [ vnet_hdr ] [ multi_queue ]

Where: USER  := { STRING | NUMBER }
       GROUP := { STRING | NUMBER }`
)

func (cmd cmd) tuntap() error {
	if !cmd.tokenRemains() {
		return cmd.tuntapShow()
	}

	c := cmd.findPrefix("add", "del", "show", "list", "lst", "help")

	options, err := cmd.parseTunTap()
	if err != nil {
		return err
	}

	switch c {
	case "add":
		return cmd.tuntapAdd(options)
	case "del":
		return cmd.tuntapDel(options)
	case "show", "list", "lst":
		return cmd.tuntapShow()
	case "help":
		fmt.Fprint(cmd.out, tuntapHelp)

		return nil
	default:
		return cmd.usage()
	}
}

type tuntapOptions struct {
	mode  netlink.TuntapMode
	user  int
	group int
	name  string
	flags netlink.TuntapFlag
}

var defaultTuntapOptions = tuntapOptions{
	mode:  netlink.TUNTAP_MODE_TUN,
	user:  -1,
	group: -1,
	name:  "",
	flags: netlink.TUNTAP_DEFAULTS,
}

func (cmd cmd) parseTunTap() (tuntapOptions, error) {
	var err error

	options := defaultTuntapOptions

	for cmd.tokenRemains() {
		switch cmd.findPrefix("mode", "user", "group", "one_queue", "pi", "vnet_hdr", "multi_queue", "name", "dev") {
		case "mode":
			switch cmd.nextToken("tun, tap") {
			case "tun":
				options.mode = netlink.TUNTAP_MODE_TUN
			case "tap":
				options.mode = netlink.TUNTAP_MODE_TAP
			default:
				return tuntapOptions{}, fmt.Errorf("invalid mode %s", cmd.currentToken())
			}
		case "user":
			options.user, err = parseValue[int](cmd, "USER")
			if err != nil {
				return tuntapOptions{}, err
			}
		case "group":
			options.group, err = parseValue[int](cmd, "GROUP")
			if err != nil {
				return tuntapOptions{}, err
			}
		case "dev", "name":
			options.name, err = parseValue[string](cmd, "NAME")
			if err != nil {
				return tuntapOptions{}, err
			}
		case "one_queue":
			options.flags |= netlink.TUNTAP_ONE_QUEUE
		case "pi":
			options.flags &^= netlink.TUNTAP_NO_PI
		case "vnet_hdr":
			options.flags |= netlink.TUNTAP_VNET_HDR
		case "multi_queue":
			options.flags |= netlink.TUNTAP_MULTI_QUEUE_DEFAULTS
			options.flags &^= netlink.TUNTAP_ONE_QUEUE

		default:
			return tuntapOptions{}, cmd.usage()
		}
	}

	return options, nil
}

func (cmd cmd) tuntapAdd(options tuntapOptions) error {
	link := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{
			Name: options.name,
		},
		Mode: options.mode,
	}

	if options.user >= 0 && options.user <= math.MaxUint16 {
		link.Owner = uint32(options.user)
	}

	if options.group >= 0 && options.group <= math.MaxUint16 {
		link.Group = uint32(options.group)
	}

	link.Flags = options.flags

	if err := cmd.handle.LinkAdd(link); err != nil {
		return err
	}

	return nil
}

func (cmd cmd) tuntapDel(options tuntapOptions) error {
	links, err := cmd.handle.LinkList()
	if err != nil {
		return err
	}

	filteredTunTaps := make([]*netlink.Tuntap, 0)

	for _, link := range links {
		tunTap, ok := link.(*netlink.Tuntap)
		if !ok {
			continue
		}

		if options.name != "" && tunTap.Name != options.name {
			continue
		}

		if options.mode != 0 && tunTap.Mode != options.mode {
			continue
		}

		filteredTunTaps = append(filteredTunTaps, tunTap)
	}

	if len(filteredTunTaps) != 1 {
		return fmt.Errorf("found %d matching tun/tap devices", len(filteredTunTaps))
	}

	if err := cmd.handle.LinkDel(filteredTunTaps[0]); err != nil {
		return err
	}

	return nil
}

type Tuntap struct {
	IfName string   `json:"ifname"`
	Flags  []string `json:"flags"`
}

func (cmd cmd) tuntapShow() error {
	links, err := cmd.handle.LinkList()
	if err != nil {
		return err
	}

	prints := make([]Tuntap, 0)

	for _, link := range links {
		tunTap, ok := link.(*netlink.Tuntap)
		if !ok {
			continue
		}

		var obj Tuntap

		obj.Flags = append(obj.Flags, tunTap.Mode.String())

		if tunTap.Flags&netlink.TUNTAP_NO_PI == 1 {
			obj.Flags = append(obj.Flags, "pi")
		}

		if tunTap.Flags&netlink.TUNTAP_ONE_QUEUE != 0 {
			obj.Flags = append(obj.Flags, "one_queue")
		} else if tunTap.Flags&netlink.TUNTAP_MULTI_QUEUE != 0 {
			obj.Flags = append(obj.Flags, "multi_queue")
		}

		if tunTap.Flags&netlink.TUNTAP_VNET_HDR != 0 {
			obj.Flags = append(obj.Flags, "vnet_hdr")
		}

		if tunTap.NonPersist {
			obj.Flags = append(obj.Flags, "non-persist")
		} else {
			obj.Flags = append(obj.Flags, "persist")
		}

		if tunTap.Owner != 0 {
			obj.Flags = append(obj.Flags, fmt.Sprintf("user %d", tunTap.Owner))
		}

		if tunTap.Group != 0 {
			obj.Flags = append(obj.Flags, fmt.Sprintf("group %d", tunTap.Group))
		}

		obj.IfName = tunTap.Name

		prints = append(prints, obj)
	}

	if cmd.opts.json {
		return printJSON(cmd, prints)
	}

	for _, print := range prints {
		output := fmt.Sprintf("%s:", print.IfName)

		for _, flag := range print.Flags {
			output += fmt.Sprintf(" %s", flag)
		}

		fmt.Fprintln(cmd.out, output)
	}

	return nil
}