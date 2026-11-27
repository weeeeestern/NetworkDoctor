//go:build linux

package bpf

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/vishvananda/netlink"
)

type nopCloser struct{ name string }

func (nopCloser) Close() error { return nil }

func TestPreferTCX(t *testing.T) {
	tcxLinks := []io.Closer{nopCloser{"tcx"}}
	clsactLinks := []io.Closer{nopCloser{"clsact"}}
	clsact := func() ([]io.Closer, error) { return clsactLinks, nil }

	t.Run("tcx works", func(t *testing.T) {
		got, mode, err := preferTCX(func() ([]io.Closer, error) { return tcxLinks, nil }, clsact)
		if err != nil || mode != "tcx" || got[0] != tcxLinks[0] {
			t.Fatalf("got %v %q %v, want tcx links", got, mode, err)
		}
	})

	t.Run("kernel without tcx falls back to clsact", func(t *testing.T) {
		notSupported := func() ([]io.Closer, error) {
			return nil, fmt.Errorf("tcx: %w", ebpf.ErrNotSupported)
		}
		got, mode, err := preferTCX(notSupported, clsact)
		if err != nil || mode != "clsact" || got[0] != clsactLinks[0] {
			t.Fatalf("got %v %q %v, want clsact links", got, mode, err)
		}
	})

	t.Run("other tcx errors also fall back to clsact", func(t *testing.T) {
		boom := errors.New("permission denied")
		got, mode, err := preferTCX(func() ([]io.Closer, error) { return nil, boom }, clsact)
		if err != nil || mode != "clsact" || got[0] != clsactLinks[0] {
			t.Fatalf("got %v %q %v, want clsact links", got, mode, err)
		}
	})

	t.Run("clsact error is returned", func(t *testing.T) {
		boom := errors.New("no such device")
		notSupported := func() ([]io.Closer, error) { return nil, ebpf.ErrNotSupported }
		_, _, err := preferTCX(notSupported, func() ([]io.Closer, error) { return nil, boom })
		if !errors.Is(err, boom) {
			t.Fatalf("got %v, want %v", err, boom)
		}
	})
}

func TestStaleNDFilters(t *testing.T) {
	filters := []netlink.Filter{
		&netlink.BpfFilter{Name: "nd_tc_ingress"},
		&netlink.BpfFilter{Name: "cil_from_netdev"},
		&netlink.U32{},
		&netlink.BpfFilter{Name: "nd_tc_egress"},
	}
	got := staleNDFilters(filters)
	if len(got) != 2 {
		t.Fatalf("got %d filters, want 2", len(got))
	}
	for _, f := range got {
		if f.Name != "nd_tc_ingress" && f.Name != "nd_tc_egress" {
			t.Fatalf("unexpected filter %q", f.Name)
		}
	}
}
