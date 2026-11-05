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
		seg := func(bytes int64) hls.Media {
			return hls.Media{Init: "init.mp4", Segments: []hls.Segment{{URI: "seg_000.m4s", Duration: 2, Bytes: bytes}}}
		}
		j.HLS = &hls.Output{Renditions: []hls.Rendition{
			{Name: "360p", Width: 640, Height: 360, Codecs: "avc1.640028", MaxRate: 800_000, Media: seg(100_000)},
			{Name: "720p", Width: 1280, Height: 720, Codecs: "avc1.640028", MaxRate: 2_800_000, Media: seg(1_000_000)},
		}}
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
	parts := g.playable("720p")
	if len(parts) != 2 {
		t.Fatalf("%d parts, want 2 (stop at the gap)", len(parts))
	}
	if parts[1].Dir != "v/1/1/720p" {
		t.Errorf("part dir = %q, want the rung's directory relative to the job's prefix", parts[1].Dir)
	}
	if len(g.playable("1080p")) != 0 {
		t.Error("a rung chunk 0 never published is playable")
	}
}

func TestPlayableStopsAtChunkMissingRung(t *testing.T) {
	odd := chunkJob(1, jobs.StateDone)
	odd.HLS.Renditions = odd.HLS.Renditions[:1] // only 360p
	g := graph{root: jobs.Job{ID: "root"}, chunks: []jobs.Job{chunkJob(0, jobs.StateDone), odd}}
	if n := len(g.playable("720p")); n != 1 {
		t.Errorf("720p has %d parts, want 1 (stop where the rung is missing)", n)
	}
}

func TestPreLadderChunkPlays(t *testing.T) {
	key := "proxies/root/v/0/1"
	old := jobs.Job{Kind: jobs.KindChunk, State: jobs.StateDone, ChunkIndex: new(int), OutputKey: &key,
		HLS: &hls.Output{Media: hls.Media{Init: "init.mp4", Segments: []hls.Segment{{URI: "seg_000.m4s", Duration: 2}}}}}
	g := graph{root: jobs.Job{ID: "root"}, chunks: []jobs.Job{old}}
	if !g.ready() || g.top() != "" {
		t.Fatalf("ready=%v top=%q, want ready with one unnamed rendition", g.ready(), g.top())
	}
	if p := g.playable(""); len(p) != 1 || p[0].Dir != "v/0/1" {
		t.Errorf("parts %+v", p)
	}
	if v := g.variants(); len(v) != 1 || v[0].URI != "video.m3u8" || v[0].Bandwidth != legacyBandwidth {
		t.Errorf("variants %+v", v)
	}
}

func TestVariantsBandwidth(t *testing.T) {
	audioKey := "proxies/root/a/1"
	audio := jobs.Job{Kind: jobs.KindAudio, State: jobs.StateDone, OutputKey: &audioKey,
		HLS: &hls.Output{Media: hls.Media{Init: "init.mp4", Segments: []hls.Segment{{URI: "a.m4s", Duration: 2, Bytes: 32_000}}}}}
	g := graph{root: jobs.Job{ID: "root"}, chunks: []jobs.Job{chunkJob(0, jobs.StateDone)}, audio: &audio}
	v := g.variants()
	if len(v) != 2 || v[0].URI != "video_360p.m3u8" || v[1].URI != "video_720p.m3u8" {
		t.Fatalf("variants %+v", v)
	}
	// 360p measured 400 kbit/s, under its 800k cap: the cap counts. Audio 128k.
	if v[0].Bandwidth != 800_000+128_000 || v[0].AverageBandwidth != 400_000+128_000 {
		t.Errorf("360p bandwidth %d avg %d", v[0].Bandwidth, v[0].AverageBandwidth)
	}
	// 720p measured 4 Mbit/s, over its 2.8M cap: the measurement counts.
	if v[1].Bandwidth != 4_000_000+128_000 || v[1].Width != 1280 {
		t.Errorf("720p %+v", v[1])
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
