//go:build linux

package main

import (
	"context"
	"os"
	"sync/atomic"
	"time"
)

// drill feeds the matcher one drillLine each time path appears (an
// exec'd `touch`), so the whole remedy -- the verdict, the readiness
// probe, the janitor's delete, the alert -- can be exercised on a live
// runner without waiting for upstream #4307 to strike. The line takes the
// same road as the runner's own output: during a job it is only counted,
// and an idle runner is reported stuck after the usual grace. Removing
// the file and creating it again feeds another line.
func drill(ctx context.Context, path string, every time.Duration, lines chan<- string, dropped *atomic.Uint64) {
	t := time.NewTicker(every)
	defer t.Stop()
	present := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, err := os.Stat(path)
			now := err == nil
			if now && !present {
				logf("drill: %s appeared; feeding a simulated stuck signature", path)
				select {
				case lines <- drillLine:
				default:
					dropped.Add(1)
				}
			}
			present = now
		}
	}
}
