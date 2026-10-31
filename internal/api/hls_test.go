package api

import (
	"fmt"
	"testing"

	"github.com/alexanderritik/reelay/internal/hls"
	"github.com/alexanderritik/reelay/internal/jobs"
)

func chunkJob(i int, state jobs.State) jobs.Job {
	j := jobs.Job{Kind: jobs.KindChunk, State: state, ChunkIndex: &i}
	if state == jobs.StateDone {
		key := fmt.Sprintf("proxies/root/v/%d/1", i)
		j.OutputKey = &key
		j.HLS = &hls.Media{Init: "init.mp4", Segments: []hls.Segment{{URI: "seg_000.m4s", Duration: 2}}}
	}
	return j
}

func TestPlayableStopsAtFirstUnfinishedChunk(t *testing.T) {
	g := graph{root: jobs.Job{ID: "root"}, chunks: []jobs.Job{
		chunkJob(0, jobs.StateDone),
		chunkJob(1, jobs.StateDone),
		chunkJob(2, jobs.StateRunning), // a retry in progress
		chunkJob(3, jobs.StateDone),    // finished early, but must wait for 2
	}}
	parts := g.playable()
	if len(parts) != 2 {
		t.Fatalf("%d parts, want 2 (stop at the gap)", len(parts))
	}
	if parts[1].Dir != "v/1/1" {
		t.Errorf("part dir = %q, want relative to the job's prefix", parts[1].Dir)
	}
}

func TestReadyWaitsForAudioAndFirstChunk(t *testing.T) {
	audio := jobs.Job{Kind: jobs.KindAudio, State: jobs.StateRunning}
	g := graph{chunks: []jobs.Job{chunkJob(0, jobs.StateDone)}, audio: &audio}
	if g.ready() {
		t.Error("ready before audio committed")
	}
	audio.State = jobs.StateDone
	if !g.ready() {
		t.Error("not ready with audio and chunk 0 in")
	}
	if (graph{chunks: []jobs.Job{chunkJob(0, jobs.StateQueued)}}).ready() {
		t.Error("ready before chunk 0 committed")
	}
}
