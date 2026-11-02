package im

import (
	"context"
	"time"

	"testing"

	"github.com/harrymitchinson/oikade/internal/matter/tlv"
	"github.com/harrymitchinson/oikade/internal/matter/transport"
)

// grantView lets fabric 1's node 7 view, and nothing else.
type grantView struct{}

func (grantView) Allowed(s *Subject, _ *Endpoint, _ uint32, p Privilege) bool {
	return s.FabricIndex == 1 && s.NodeID == 7 && PrivilegeView.Grants(p)
}

func TestPrivilegeGrants(t *testing.T) {
	all := []Privilege{PrivilegeView, PrivilegeProxyView, PrivilegeOperate, PrivilegeManage, PrivilegeAdminister}
	want := map[Privilege][]Privilege{
		PrivilegeView:       {PrivilegeView},
		PrivilegeProxyView:  {PrivilegeView, PrivilegeProxyView},
		PrivilegeOperate:    {PrivilegeView, PrivilegeOperate},
		PrivilegeManage:     {PrivilegeView, PrivilegeOperate, PrivilegeManage},
		PrivilegeAdminister: all,
	}
	for _, held := range all {
		for _, w := range all {
			granted := false
			for _, g := range want[held] {
				granted = granted || g == w
			}
			if held.Grants(w) != granted {
				t.Errorf("%d grants %d: %v, want %v", held, w, held.Grants(w), granted)
			}
		}
	}
}

// accessNode has an on/off cluster with a writable attribute and a command.
func accessNode() *Node {
	on := false
	c := &Cluster{ID: 6, Revision: 1,
		Attributes: []Attribute{{ID: 0,
			Read: func(_ *Request, w *tlv.Writer, tag tlv.Tag) error { w.PutBool(tag, on); return nil },
			Write: func(_ *Request, r *tlv.Reader, _ WriteOp) Status {
				v, err := r.Bool()
				if err != nil {
					return StatusInvalidDataType
				}
				on = v
				return StatusSuccess
			},
		}},
		Commands: []Command{{ID: 1, Invoke: func(*Request, *tlv.Reader) (*Response, error) { on = true; return nil, nil }}},
	}
	node := &Node{}
	_ = node.AddEndpoint(&Endpoint{ID: 1, Clusters: []*Cluster{c}})
	return node
}

// TestAccessDenied: a session without the privilege gets UNSUPPORTED_ACCESS for reads, writes
// and invokes, and a wildcard read leaves out what it may not see.
func TestAccessDenied(t *testing.T) {
	for name, cfg := range map[string]transport.SecureSessionConfig{
		"CASE on another fabric": {Auth: transport.AuthCASE, FabricIndex: 2, PeerNodeID: 7},
		"CASE, another node":     {Auth: transport.AuthCASE, FabricIndex: 1, PeerNodeID: 8},
		"no authentication":      {},
	} {
		t.Run(name, func(t *testing.T) {
			client, _, _, _ := pairAs(t, accessNode(), grantView{}, cfg)
			ctx := timeoutCtx(t)
			path := AttributePath{Endpoint: 1, Cluster: 6, Attribute: 0}
			reports, err := client.Read(ctx, path)
			if err != nil || len(reports) != 1 || reports[0].Status != StatusUnsupportedAccess {
				t.Errorf("read: %+v, %v", reports, err)
			}
			reports, err = client.Read(ctx, AttributePath{WildEndpoint: true, WildCluster: true, WildAttribute: true})
			if err != nil || len(reports) != 0 {
				t.Errorf("wildcard read: %+v, %v", reports, err)
			}
			st, err := client.Write(ctx, path, func(w *tlv.Writer, tag tlv.Tag) { w.PutBool(tag, true) })
			if err != nil || st != StatusUnsupportedAccess {
				t.Errorf("write: %#x, %v", st, err)
			}
			res, err := client.Invoke(ctx, CommandPath{Endpoint: 1, Cluster: 6, Command: 1}, nil, false, 0)
			if err != nil || res.Status != StatusUnsupportedAccess {
				t.Errorf("invoke: %+v, %v", res, err)
			}
		})
	}
}

// TestAccessGranted: View lets a CASE session read, but not write or invoke, which need Operate.
func TestAccessGranted(t *testing.T) {
	client, _, _, _ := pairAs(t, accessNode(), grantView{}, transport.SecureSessionConfig{Auth: transport.AuthCASE, FabricIndex: 1, PeerNodeID: 7})
	ctx := timeoutCtx(t)
	path := AttributePath{Endpoint: 1, Cluster: 6, Attribute: 0}
	reports, err := client.Read(ctx, path)
	if err != nil || len(reports) != 1 || reports[0].Status != StatusSuccess {
		t.Errorf("read: %+v, %v", reports, err)
	}
	if st, err := client.Write(ctx, path, func(w *tlv.Writer, tag tlv.Tag) { w.PutBool(tag, true) }); err != nil || st != StatusUnsupportedAccess {
		t.Errorf("write: %#x, %v", st, err)
	}
	if res, err := client.Invoke(ctx, CommandPath{Endpoint: 1, Cluster: 6, Command: 1}, nil, false, 0); err != nil || res.Status != StatusUnsupportedAccess {
		t.Errorf("invoke: %+v, %v", res, err)
	}
}

func timeoutCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}
