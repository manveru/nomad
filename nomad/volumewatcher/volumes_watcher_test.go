package volumewatcher

import (
	"context"
	"testing"
	"time"

	memdb "github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/stretchr/testify/require"
)

// TestVolumeWatch_EnableDisable tests the watcher registration logic that needs
// to happen during leader step-up/step-down
func TestVolumeWatch_EnableDisable(t *testing.T) {
	t.Parallel()
	require := require.New(t)

	srv := &MockRPCServer{}
	srv.state = state.TestStateStore(t)
	index := uint64(100)

	watcher := NewVolumesWatcher(testlog.HCLogger(t),
		srv, srv,
		LimitStateQueriesPerSecond,
		CrossVolumeUpdateBatchDuration)

	watcher.SetEnabled(true, srv.State())

	plugin := mock.CSIPlugin()
	node := testNode(nil, plugin, srv.State())
	alloc := mock.Alloc()
	alloc.ClientStatus = structs.AllocClientStatusComplete
	vol := testVolume(nil, plugin, alloc, node.ID)

	index++
	err := srv.State().CSIVolumeRegister(index, []*structs.CSIVolume{vol})
	require.NoError(err)

	claim := &structs.CSIVolumeClaim{Mode: structs.CSIVolumeClaimRelease}
	index++
	err = srv.State().CSIVolumeClaim(index, vol.Namespace, vol.ID, claim)
	require.NoError(err)
	require.Eventually(func() bool {
		return 1 == len(watcher.watchers)
	}, time.Second, 10*time.Millisecond)

	watcher.SetEnabled(false, srv.State())
	require.Equal(0, len(watcher.watchers))
}

// TestVolumeWatch_Checkpoint tests the checkpointing of progress across
// leader leader step-up/step-down
func TestVolumeWatch_Checkpoint(t *testing.T) {
	t.Parallel()
	require := require.New(t)

	srv := &MockRPCServer{}
	srv.state = state.TestStateStore(t)
	index := uint64(100)

	watcher := NewVolumesWatcher(testlog.HCLogger(t),
		srv, srv,
		LimitStateQueriesPerSecond,
		CrossVolumeUpdateBatchDuration)

	plugin := mock.CSIPlugin()
	node := testNode(nil, plugin, srv.State())
	alloc := mock.Alloc()
	alloc.ClientStatus = structs.AllocClientStatusComplete
	vol := testVolume(nil, plugin, alloc, node.ID)

	watcher.SetEnabled(true, srv.State())

	index++
	err := srv.State().CSIVolumeRegister(index, []*structs.CSIVolume{vol})
	require.NoError(err)

	// we should get or start up a watcher when we get an update for
	// the volume from the state store
	require.Eventually(func() bool {
		return 1 == len(watcher.watchers)
	}, time.Second, 10*time.Millisecond)

	// step-down (this is sync, but step-up is async)
	watcher.SetEnabled(false, srv.State())
	require.Equal(0, len(watcher.watchers))

	// step-up again
	watcher.SetEnabled(true, srv.State())
	require.Eventually(func() bool {
		return 1 == len(watcher.watchers) &&
			!watcher.watchers[vol.ID+vol.Namespace].isRunning()
	}, time.Second, 10*time.Millisecond)
}

// TestVolumeWatch_StartStop tests the start and stop of the watcher when
// it receives notifcations and has completed its work
func TestVolumeWatch_StartStop(t *testing.T) {
	t.Parallel()
	require := require.New(t)

	ctx, exitFn := context.WithCancel(context.Background())
	defer exitFn()

	srv := &MockStatefulRPCServer{}
	srv.state = state.TestStateStore(t)
	index := uint64(100)
	srv.volumeUpdateBatcher = NewVolumeUpdateBatcher(
		ctx, CrossVolumeUpdateBatchDuration, srv)

	watcher := NewVolumesWatcher(testlog.HCLogger(t),
		srv, srv,
		LimitStateQueriesPerSecond,
		CrossVolumeUpdateBatchDuration)

	watcher.SetEnabled(true, srv.State())
	require.Equal(0, len(watcher.watchers))

	plugin := mock.CSIPlugin()
	node := testNode(nil, plugin, srv.State())
	alloc1 := mock.Alloc()
	alloc1.ClientStatus = structs.AllocClientStatusRunning
	alloc2 := mock.Alloc()
	alloc2.Job = alloc1.Job
	alloc2.ClientStatus = structs.AllocClientStatusRunning
	index++
	err := srv.State().UpsertJob(index, alloc1.Job)
	require.NoError(err)
	index++
	err = srv.State().UpsertAllocs(index, []*structs.Allocation{alloc1, alloc2})
	require.NoError(err)

	// register a volume
	vol := testVolume(nil, plugin, alloc1, node.ID)
	index++
	err = srv.State().CSIVolumeRegister(index, []*structs.CSIVolume{vol})
	require.NoError(err)

	// assert we get a watcher; there are no claims so it should immediately stop
	require.Eventually(func() bool {
		return 1 == len(watcher.watchers) &&
			!watcher.watchers[vol.ID+vol.Namespace].isRunning()
	}, time.Second*2, 10*time.Millisecond)

	// claim the volume for both allocs
	claim := &structs.CSIVolumeClaim{
		AllocationID: alloc1.ID,
		NodeID:       node.ID,
		Mode:         structs.CSIVolumeClaimRead,
	}
	index++
	err = srv.State().CSIVolumeClaim(index, vol.Namespace, vol.ID, claim)
	require.NoError(err)
	claim.AllocationID = alloc2.ID
	index++
	err = srv.State().CSIVolumeClaim(index, vol.Namespace, vol.ID, claim)
	require.NoError(err)

	// reap the volume and assert nothing has happened
	claim = &structs.CSIVolumeClaim{
		AllocationID: alloc1.ID,
		NodeID:       node.ID,
		Mode:         structs.CSIVolumeClaimRelease,
	}
	index++
	err = srv.State().CSIVolumeClaim(index, vol.Namespace, vol.ID, claim)
	require.NoError(err)

	ws := memdb.NewWatchSet()
	vol, _ = srv.State().CSIVolumeByID(ws, vol.Namespace, vol.ID)
	require.Equal(2, len(vol.ReadAllocs))

	// alloc becomes terminal
	alloc1.ClientStatus = structs.AllocClientStatusComplete
	index++
	err = srv.State().UpsertAllocs(index, []*structs.Allocation{alloc1})
	require.NoError(err)
	index++
	claim.State = structs.CSIVolumeClaimStateReadyToFree
	err = srv.State().CSIVolumeClaim(index, vol.Namespace, vol.ID, claim)
	require.NoError(err)

	// 1 claim has been released and watcher stops
	require.Eventually(func() bool {
		ws := memdb.NewWatchSet()
		vol, _ := srv.State().CSIVolumeByID(ws, vol.Namespace, vol.ID)
		return len(vol.ReadAllocs) == 1 && len(vol.PastClaims) == 0
	}, time.Second*2, 10*time.Millisecond)

	require.Eventually(func() bool {
		return !watcher.watchers[vol.ID+vol.Namespace].isRunning()
	}, time.Second*5, 10*time.Millisecond)

	// the watcher will have incremented the index so we need to make sure
	// our inserts will trigger new events
	index, _ = srv.State().LatestIndex()

	// remaining alloc's job is stopped (alloc is not marked terminal)
	alloc2.Job.Stop = true
	index++
	err = srv.State().UpsertJob(index, alloc2.Job)
	require.NoError(err)

	// job deregistration write a claim with no allocations or nodes
	claim = &structs.CSIVolumeClaim{
		Mode: structs.CSIVolumeClaimRelease,
	}
	index++
	err = srv.State().CSIVolumeClaim(index, vol.Namespace, vol.ID, claim)
	require.NoError(err)

	// all claims have been released and watcher has stopped again
	require.Eventually(func() bool {
		ws := memdb.NewWatchSet()
		vol, _ := srv.State().CSIVolumeByID(ws, vol.Namespace, vol.ID)
		return len(vol.ReadAllocs) == 0 && len(vol.PastClaims) == 0
	}, time.Second*2, 10*time.Millisecond)

	require.Eventually(func() bool {
		return !watcher.watchers[vol.ID+vol.Namespace].isRunning()
	}, time.Second*5, 10*time.Millisecond)
}

// TestVolumeWatch_RegisterDeregister tests the start and stop of
// watchers around registration
func TestVolumeWatch_RegisterDeregister(t *testing.T) {
	t.Parallel()
	require := require.New(t)

	ctx, exitFn := context.WithCancel(context.Background())
	defer exitFn()

	srv := &MockStatefulRPCServer{}
	srv.state = state.TestStateStore(t)
	srv.volumeUpdateBatcher = NewVolumeUpdateBatcher(
		ctx, CrossVolumeUpdateBatchDuration, srv)

	index := uint64(100)

	watcher := NewVolumesWatcher(testlog.HCLogger(t),
		srv, srv,
		LimitStateQueriesPerSecond,
		CrossVolumeUpdateBatchDuration)

	watcher.SetEnabled(true, srv.State())
	require.Equal(0, len(watcher.watchers))

	plugin := mock.CSIPlugin()
	node := testNode(nil, plugin, srv.State())
	alloc := mock.Alloc()
	alloc.ClientStatus = structs.AllocClientStatusComplete

	// register a volume
	vol := testVolume(nil, plugin, alloc, node.ID)
	index++
	err := srv.State().CSIVolumeRegister(index, []*structs.CSIVolume{vol})
	require.NoError(err)

	require.Eventually(func() bool {
		return 1 == len(watcher.watchers)
	}, time.Second, 10*time.Millisecond)

	// reap the volume and assert we've cleaned up
	w := watcher.watchers[vol.ID+vol.Namespace]
	w.Notify(vol)

	require.Eventually(func() bool {
		ws := memdb.NewWatchSet()
		vol, _ := srv.State().CSIVolumeByID(ws, vol.Namespace, vol.ID)
		return len(vol.ReadAllocs) == 0 && len(vol.PastClaims) == 0
	}, time.Second*2, 10*time.Millisecond)

	require.Eventually(func() bool {
		return !watcher.watchers[vol.ID+vol.Namespace].isRunning()
	}, time.Second*1, 10*time.Millisecond)

	require.Equal(1, srv.countCSINodeDetachVolume, "node detach RPC count")
	require.Equal(1, srv.countCSIControllerDetachVolume, "controller detach RPC count")
	require.Equal(2, srv.countUpsertVolumeClaims, "upsert claims count")

	// deregistering the volume doesn't cause an update that triggers
	// a watcher; we'll clean up this watcher in a GC later
	err = srv.State().CSIVolumeDeregister(index, vol.Namespace, []string{vol.ID})
	require.NoError(err)
	require.Equal(1, len(watcher.watchers))
	require.False(watcher.watchers[vol.ID+vol.Namespace].isRunning())
}
