/*
   Copyright 2014 CoreOS, Inc.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package raft

import (
	"reflect"
	"testing"
	"time"

	"github.com/coreos/etcd/Godeps/_workspace/src/code.google.com/p/go.net/context"
	"github.com/coreos/etcd/pkg/testutil"
	"github.com/coreos/etcd/raft/raftpb"
)

// TestNodeStep ensures that node.Step sends msgProp to propc chan
// and other kinds of messages to recvc chan.
func TestNodeStep(t *testing.T) {
	for i, msgn := range raftpb.MessageType_name {
		n := &node{
			propc: make(chan raftpb.Message, 1),
			recvc: make(chan raftpb.Message, 1),
		}
		msgt := raftpb.MessageType(i)
		n.Step(context.TODO(), raftpb.Message{Type: msgt})
		// Proposal goes to proc chan. Others go to recvc chan.
		if msgt == raftpb.MsgProp {
			select {
			case <-n.propc:
			default:
				t.Errorf("%d: cannot receive %s on propc chan", msgt, msgn)
			}
		} else {
			if msgt == raftpb.MsgBeat || msgt == raftpb.MsgHup {
				select {
				case <-n.recvc:
					t.Errorf("%d: step should ignore %s", msgt, msgn)
				default:
				}
			} else {
				select {
				case <-n.recvc:
				default:
					t.Errorf("%d: cannot receive %s on recvc chan", msgt, msgn)
				}
			}
		}
	}
}

// Cancel and Stop should unblock Step()
func TestNodeStepUnblock(t *testing.T) {
	// a node without buffer to block step
	n := &node{
		propc: make(chan raftpb.Message),
		done:  make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopFunc := func() { close(n.done) }

	tests := []struct {
		unblock func()
		werr    error
	}{
		{stopFunc, ErrStopped},
		{cancel, context.Canceled},
	}

	for i, tt := range tests {
		errc := make(chan error, 1)
		go func() {
			err := n.Step(ctx, raftpb.Message{Type: raftpb.MsgProp})
			errc <- err
		}()
		tt.unblock()
		select {
		case err := <-errc:
			if err != tt.werr {
				t.Errorf("#%d: err = %v, want %v", i, err, tt.werr)
			}
			//clean up side-effect
			if ctx.Err() != nil {
				ctx = context.TODO()
			}
			select {
			case <-n.done:
				n.done = make(chan struct{})
			default:
			}
		case <-time.After(time.Millisecond * 100):
			t.Errorf("#%d: failed to unblock step", i)
		}
	}
}

// TestNodePropose ensures that node.Propose sends the given proposal to the underlying raft.
func TestNodePropose(t *testing.T) {
	msgs := []raftpb.Message{}
	appendStep := func(r *raft, m raftpb.Message) {
		msgs = append(msgs, m)
	}

	n := newNode()
	r := newRaft(1, []uint64{1}, 10, 1)
	go n.run(r)
	n.Campaign(context.TODO())
	for {
		rd := <-n.Ready()
		// change the step function to appendStep until this raft becomes leader
		if rd.SoftState.Lead == r.id {
			r.step = appendStep
			n.Advance()
			break
		}
		n.Advance()
	}
	n.Propose(context.TODO(), []byte("somedata"))
	n.Stop()

	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want %d", len(msgs), 1)
	}
	if msgs[0].Type != raftpb.MsgProp {
		t.Errorf("msg type = %d, want %d", msgs[0].Type, raftpb.MsgProp)
	}
	if !reflect.DeepEqual(msgs[0].Entries[0].Data, []byte("somedata")) {
		t.Errorf("data = %v, want %v", msgs[0].Entries[0].Data, []byte("somedata"))
	}
}

// TestNodeProposeConfig ensures that node.ProposeConfChange sends the given configuration proposal
// to the underlying raft.
func TestNodeProposeConfig(t *testing.T) {
	msgs := []raftpb.Message{}
	appendStep := func(r *raft, m raftpb.Message) {
		msgs = append(msgs, m)
	}

	n := newNode()
	r := newRaft(1, []uint64{1}, 10, 1)
	go n.run(r)
	n.Campaign(context.TODO())
	for {
		rd := <-n.Ready()
		// change the step function to appendStep until this raft becomes leader
		if rd.SoftState.Lead == r.id {
			r.step = appendStep
			n.Advance()
			break
		}
		n.Advance()
	}
	cc := raftpb.ConfChange{Type: raftpb.ConfChangeAddNode, NodeID: 1}
	ccdata, err := cc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	n.ProposeConfChange(context.TODO(), cc)
	n.Stop()

	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want %d", len(msgs), 1)
	}
	if msgs[0].Type != raftpb.MsgProp {
		t.Errorf("msg type = %d, want %d", msgs[0].Type, raftpb.MsgProp)
	}
	if !reflect.DeepEqual(msgs[0].Entries[0].Data, ccdata) {
		t.Errorf("data = %v, want %v", msgs[0].Entries[0].Data, ccdata)
	}
}

// TestBlockProposal ensures that node will block proposal when it does not
// know who is the current leader; node will accept proposal when it knows
// who is the current leader.
func TestBlockProposal(t *testing.T) {
	n := newNode()
	r := newRaft(1, []uint64{1}, 10, 1)
	go n.run(r)
	defer n.Stop()

	errc := make(chan error, 1)
	go func() {
		errc <- n.Propose(context.TODO(), []byte("somedata"))
	}()

	testutil.ForceGosched()
	select {
	case err := <-errc:
		t.Errorf("err = %v, want blocking", err)
	default:
	}

	n.Campaign(context.TODO())
	testutil.ForceGosched()
	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("err = %v, want %v", err, nil)
		}
	default:
		t.Errorf("blocking proposal, want unblocking")
	}
}

// TestNodeTick ensures that node.Tick() will increase the
// elapsed of the underlying raft state machine.
func TestNodeTick(t *testing.T) {
	n := newNode()
	r := newRaft(1, []uint64{1}, 10, 1)
	go n.run(r)
	elapsed := r.elapsed
	n.Tick()
	n.Stop()
	if r.elapsed != elapsed+1 {
		t.Errorf("elapsed = %d, want %d", r.elapsed, elapsed+1)
	}
}

// TestNodeStop ensures that node.Stop() blocks until the node has stopped
// processing, and that it is idempotent
func TestNodeStop(t *testing.T) {
	n := newNode()
	r := newRaft(1, []uint64{1}, 10, 1)
	donec := make(chan struct{})

	go func() {
		n.run(r)
		close(donec)
	}()

	elapsed := r.elapsed
	n.Tick()
	n.Stop()

	select {
	case <-donec:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for node to stop!")
	}

	if r.elapsed != elapsed+1 {
		t.Errorf("elapsed = %d, want %d", r.elapsed, elapsed+1)
	}
	// Further ticks should have no effect, the node is stopped.
	n.Tick()
	if r.elapsed != elapsed+1 {
		t.Errorf("elapsed = %d, want %d", r.elapsed, elapsed+1)
	}
	// Subsequent Stops should have no effect.
	n.Stop()
}

func TestReadyContainUpdates(t *testing.T) {
	tests := []struct {
		rd       Ready
		wcontain bool
	}{
		{Ready{}, false},
		{Ready{SoftState: &SoftState{Lead: 1}}, true},
		{Ready{HardState: raftpb.HardState{Vote: 1}}, true},
		{Ready{Entries: make([]raftpb.Entry, 1, 1)}, true},
		{Ready{CommittedEntries: make([]raftpb.Entry, 1, 1)}, true},
		{Ready{Messages: make([]raftpb.Message, 1, 1)}, true},
		{Ready{Snapshot: raftpb.Snapshot{Index: 1}}, true},
	}

	for i, tt := range tests {
		if g := tt.rd.containsUpdates(); g != tt.wcontain {
			t.Errorf("#%d: containUpdates = %v, want %v", i, g, tt.wcontain)
		}
	}
}

// TestNodeStart ensures that a node can be started correctly. The node should
// start with correct configuration change entries, and can accept and commit
// proposals.
func TestNodeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cc := raftpb.ConfChange{Type: raftpb.ConfChangeAddNode, NodeID: 1}
	ccdata, err := cc.Marshal()
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}
	wants := []Ready{
		{
			SoftState: &SoftState{Lead: 1, Nodes: []uint64{1}, RaftState: StateLeader},
			HardState: raftpb.HardState{Term: 1, Commit: 2},
			Entries: []raftpb.Entry{
				{},
				{Type: raftpb.EntryConfChange, Term: 1, Index: 1, Data: ccdata},
				{Term: 1, Index: 2},
			},
			CommittedEntries: []raftpb.Entry{
				{Type: raftpb.EntryConfChange, Term: 1, Index: 1, Data: ccdata},
				{Term: 1, Index: 2},
			},
		},
		{
			HardState:        raftpb.HardState{Term: 1, Commit: 3},
			Entries:          []raftpb.Entry{{Term: 1, Index: 3, Data: []byte("foo")}},
			CommittedEntries: []raftpb.Entry{{Term: 1, Index: 3, Data: []byte("foo")}},
		},
	}
	n := StartNode(1, []Peer{{ID: 1}}, 10, 1)
	n.ApplyConfChange(cc)
	n.Campaign(ctx)
	if g := <-n.Ready(); !reflect.DeepEqual(g, wants[0]) {
		t.Errorf("#%d: g = %+v,\n             w   %+v", 1, g, wants[0])
	} else {
		n.Advance()
	}

	n.Propose(ctx, []byte("foo"))
	if g := <-n.Ready(); !reflect.DeepEqual(g, wants[1]) {
		t.Errorf("#%d: g = %+v,\n             w   %+v", 2, g, wants[1])
	} else {
		n.Advance()
	}

	select {
	case rd := <-n.Ready():
		t.Errorf("unexpected Ready: %+v", rd)
	case <-time.After(time.Millisecond):
	}
}

func TestNodeRestart(t *testing.T) {
	entries := []raftpb.Entry{
		{},
		{Term: 1, Index: 1},
		{Term: 1, Index: 2, Data: []byte("foo")},
	}
	st := raftpb.HardState{Term: 1, Commit: 1}

	want := Ready{
		HardState: emptyState,
		// commit upto index commit index in st
		CommittedEntries: entries[1 : st.Commit+1],
	}

	n := RestartNode(1, 10, 1, nil, st, entries)
	if g := <-n.Ready(); !reflect.DeepEqual(g, want) {
		t.Errorf("g = %+v,\n             w   %+v", g, want)
	} else {
		n.Advance()
	}

	select {
	case rd := <-n.Ready():
		t.Errorf("unexpected Ready: %+v", rd)
	case <-time.After(time.Millisecond):
	}
}

func TestNodeRestartFromSnapshot(t *testing.T) {
	snap := &raftpb.Snapshot{
		Data:  []byte("some data"),
		Nodes: []uint64{1, 2},
		Index: 2,
		Term:  1,
	}
	entries := []raftpb.Entry{
		{Term: 1, Index: 2},
		{Term: 1, Index: 3, Data: []byte("foo")},
	}
	st := raftpb.HardState{Term: 1, Commit: 3}

	want := Ready{
		HardState: emptyState,
		// commit upto index commit index in st
		CommittedEntries: entries[1:],
	}

	n := RestartNode(1, 10, 1, snap, st, entries)
	if g := <-n.Ready(); !reflect.DeepEqual(g, want) {
		t.Errorf("g = %+v,\n             w   %+v", g, want)
	} else {
		n.Advance()
	}

	select {
	case rd := <-n.Ready():
		t.Errorf("unexpected Ready: %+v", rd)
	case <-time.After(time.Millisecond):
	}
}

// TestCompacts ensures Node.Compact creates a correct raft snapshot and compacts
// the raft log (call raft.compact)
func TestNodeCompact(t *testing.T) {
	ctx := context.Background()
	n := newNode()
	r := newRaft(1, []uint64{1}, 10, 1)
	go n.run(r)

	n.Campaign(ctx)
	n.Propose(ctx, []byte("foo"))

	w := raftpb.Snapshot{
		Term:  1,
		Index: 2, // one nop + one proposal
		Data:  []byte("a snapshot"),
		Nodes: []uint64{1},
	}

	testutil.ForceGosched()
	select {
	case <-n.Ready():
		n.Advance()
	default:
		t.Fatalf("unexpected proposal failure: unable to commit entry")
	}

	n.Compact(w.Index, w.Nodes, w.Data)
	testutil.ForceGosched()
	select {
	case rd := <-n.Ready():
		if !reflect.DeepEqual(rd.Snapshot, w) {
			t.Errorf("snap = %+v, want %+v", rd.Snapshot, w)
		}
		n.Advance()
	default:
		t.Fatalf("unexpected compact failure: unable to create a snapshot")
	}
	testutil.ForceGosched()
	// TODO: this test the run updates the snapi correctly... should be tested
	// separately with other kinds of updates
	select {
	case <-n.Ready():
		t.Fatalf("unexpected more ready")
	default:
	}
	n.Stop()

	if r.raftLog.offset != w.Index {
		t.Errorf("log.offset = %d, want %d", r.raftLog.offset, w.Index)
	}
}

func TestNodeAdvance(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n := StartNode(1, []Peer{{ID: 1}}, 10, 1)
	n.ApplyConfChange(raftpb.ConfChange{Type: raftpb.ConfChangeAddNode, NodeID: 1})
	n.Campaign(ctx)
	<-n.Ready()
	n.Propose(ctx, []byte("foo"))
	select {
	case rd := <-n.Ready():
		t.Fatalf("unexpected Ready before Advance: %+v", rd)
	case <-time.After(time.Millisecond):
	}
	n.Advance()
	select {
	case <-n.Ready():
	case <-time.After(time.Millisecond):
		t.Errorf("expect Ready after Advance, but there is no Ready available")
	}
}

func TestSoftStateEqual(t *testing.T) {
	tests := []struct {
		st *SoftState
		we bool
	}{
		{&SoftState{}, true},
		{&SoftState{Lead: 1}, false},
		{&SoftState{RaftState: StateLeader}, false},
		{&SoftState{Nodes: []uint64{1, 2}}, false},
	}
	for i, tt := range tests {
		if g := tt.st.equal(&SoftState{}); g != tt.we {
			t.Errorf("#%d, equal = %v, want %v", i, g, tt.we)
		}
	}
}

func TestIsHardStateEqual(t *testing.T) {
	tests := []struct {
		st raftpb.HardState
		we bool
	}{
		{emptyState, true},
		{raftpb.HardState{Vote: 1}, false},
		{raftpb.HardState{Commit: 1}, false},
		{raftpb.HardState{Term: 1}, false},
	}

	for i, tt := range tests {
		if isHardStateEqual(tt.st, emptyState) != tt.we {
			t.Errorf("#%d, equal = %v, want %v", i, isHardStateEqual(tt.st, emptyState), tt.we)
		}
	}
}
