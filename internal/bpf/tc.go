//go:build linux

package bpf

import (
	"errors"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// attachTC attaches the ingress/egress programs to the requested network
// interface. It prefers tcx (Linux 6.6+) and falls back to clsact filters on
// older kernels.
//
// tcx is needed when another tcx program shares the device: the kernel runs
// clsact filters only if every tcx program returned TC_ACT_UNSPEC, and Cilium's
// cil_from_netdev returns TC_ACT_OK, so a clsact ingress filter behind it never
// runs. The programs go to the head of the tcx chain and return TC_ACT_UNSPEC,
// so the packet continues to the programs behind them.
func attachTC(iface string, ingressProg, egressProg *ebpf.Program) ([]io.Closer, error) {
	dev, err := netlink.LinkByName(iface)
	if err != nil {
		return nil, fmt.Errorf("find interface: %w", err)
	}

	closers, mode, err := preferTCX(
		func() ([]io.Closer, error) { return attachTCX(dev.Attrs().Index, ingressProg, egressProg) },
		func() ([]io.Closer, error) { return attachClsact(dev.Attrs().Index, ingressProg, egressProg) },
	)
	if err != nil {
		return nil, err
	}
	if mode == "tcx" {
		// A crashed earlier agent can leave its clsact filters behind. The
		// egress one would still run after tcx and count every packet twice.
		removeStaleFilters(dev.Attrs().Index)
	}
	log.Printf("TC programs attached to %s via %s", iface, mode)
	return closers, nil
}

// preferTCX runs tcx and falls back to clsact only when the kernel has no tcx.
func preferTCX(tcx, clsact func() ([]io.Closer, error)) ([]io.Closer, string, error) {
	closers, err := tcx()
	if err == nil {
		return closers, "tcx", nil
	}
	if !errors.Is(err, ebpf.ErrNotSupported) {
		return nil, "", fmt.Errorf("attach tcx: %w", err)
	}
	closers, err = clsact()
	if err != nil {
		return nil, "", err
	}
	return closers, "clsact", nil
}

// attachTCX attaches both programs at the head of the device's tcx chains.
func attachTCX(linkIndex int, ingressProg, egressProg *ebpf.Program) ([]io.Closer, error) {
	var closers []io.Closer
	for _, hook := range []struct {
		prog   *ebpf.Program
		attach ebpf.AttachType
	}{
		{ingressProg, ebpf.AttachTCXIngress},
		{egressProg, ebpf.AttachTCXEgress},
	} {
		l, err := link.AttachTCX(link.TCXOptions{
			Interface: linkIndex,
			Program:   hook.prog,
			Attach:    hook.attach,
			Anchor:    link.Head(),
		})
		if err != nil {
			closeAll(closers)
			return nil, err
		}
		closers = append(closers, l)
	}
	return closers, nil
}

// attachClsact installs a clsact qdisc and replaces ingress/egress BPF filters.
func attachClsact(linkIndex int, ingressProg, egressProg *ebpf.Program) ([]io.Closer, error) {
	qdisc := &netlink.Clsact{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: linkIndex,
			Handle:    netlink.MakeHandle(0xffff, 0),
			Parent:    netlink.HANDLE_CLSACT,
		},
	}
	if err := netlink.QdiscAdd(qdisc); err != nil && !isExists(err) {
		return nil, fmt.Errorf("add clsact qdisc: %w", err)
	}

	ingress := tcFilter(linkIndex, netlink.HANDLE_MIN_INGRESS, 1, "nd_tc_ingress", ingressProg)
	egress := tcFilter(linkIndex, netlink.HANDLE_MIN_EGRESS, 1, "nd_tc_egress", egressProg)

	for _, filter := range []*netlink.BpfFilter{ingress, egress} {
		if err := netlink.FilterReplace(filter); err != nil {
			return nil, fmt.Errorf("replace filter %s: %w", filter.Name, err)
		}
	}

	return []io.Closer{
		tcFilterCloser{filter: ingress},
		tcFilterCloser{filter: egress},
	}, nil
}

// removeStaleFilters deletes NetworkDoctor clsact filters left on the device.
// Failures are logged: the qdisc may simply not exist.
func removeStaleFilters(linkIndex int) {
	dev, err := netlink.LinkByIndex(linkIndex)
	if err != nil {
		return
	}
	for _, parent := range []uint32{netlink.HANDLE_MIN_INGRESS, netlink.HANDLE_MIN_EGRESS} {
		filters, err := netlink.FilterList(dev, parent)
		if err != nil {
			continue
		}
		for _, f := range staleNDFilters(filters) {
			if err := netlink.FilterDel(f); err != nil && !isNotFound(err) {
				log.Printf("remove stale filter %s: %v", f.(*netlink.BpfFilter).Name, err)
				continue
			}
			log.Printf("removed stale clsact filter %s", f.(*netlink.BpfFilter).Name)
		}
	}
}

// staleNDFilters returns the BPF filters that NetworkDoctor installed.
func staleNDFilters(filters []netlink.Filter) []netlink.Filter {
	var out []netlink.Filter
	for _, f := range filters {
		if bf, ok := f.(*netlink.BpfFilter); ok && strings.HasPrefix(bf.Name, "nd_tc_") {
			out = append(out, f)
		}
	}
	return out
}

// tcFilter builds a direct-action BPF filter. Direct action means the BPF
// program returns TC_ACT_* values directly instead of using a separate action.
func tcFilter(linkIndex int, parent uint32, priority uint16, name string, prog *ebpf.Program) *netlink.BpfFilter {
	return &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: linkIndex,
			Parent:    parent,
			Handle:    netlink.MakeHandle(0, priority),
			Protocol:  unix.ETH_P_ALL,
			Priority:  priority,
		},
		Fd:           prog.FD(),
		Name:         name,
		DirectAction: true,
	}
}

// tcFilterCloser adapts netlink.FilterDel to io.Closer so TC filters can share
// the same cleanup path as cilium/ebpf links.
type tcFilterCloser struct {
	filter netlink.Filter
}

func (c tcFilterCloser) Close() error {
	err := netlink.FilterDel(c.filter)
	if err != nil && !isNotFound(err) {
		return err
	}
	return nil
}
