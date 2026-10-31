package media

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// runFFmpeg runs ffmpeg with args, which must include "-progress pipe:1".
// Cancelling ctx kills ffmpeg, which is how a worker that lost its lease
// stops burning CPU. If onProgress is not nil it gets every key=value line
// ffmpeg reports (about twice a second); it must not block.
func runFFmpeg(ctx context.Context, args []string, onProgress func(key, value string)) error {
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	dieWithParent(cmd)
	stderr := &tailBuffer{max: 4 << 10}
	cmd.Stderr = stderr
	cmd.WaitDelay = 5 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}
	// Read until ffmpeg closes stdout (it exits, or is killed on cancel).
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok && onProgress != nil {
			onProgress(k, v)
		}
	}

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("transcode cancelled: %w", context.Cause(ctx))
		}
		return fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// tailBuffer keeps only the last max bytes written, so a chatty ffmpeg cannot
// grow memory without bound while the end (where the error is) survives.
type tailBuffer struct {
	buf bytes.Buffer
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if over := t.buf.Len() - t.max; over > 0 {
		t.buf.Next(over)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return t.buf.String() }
