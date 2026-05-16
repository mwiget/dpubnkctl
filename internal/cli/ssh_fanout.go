package cli

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mwiget/dpubnkctl/internal/cluster"
	"github.com/mwiget/dpubnkctl/internal/ssh"
)

// runOnHostsParallel SSHes every host in plan and runs the same remote
// command, returning the failures as "<host>: <msg>" strings. Used by
// the two cluster_up parallel fan-outs (preCreateKubeDir and
// restartContainerdOnHosts) that previously each re-implemented the
// same WaitGroup + mutex + failures-slice shape with a one-line
// difference in the remote command.
//
// perOK is called for each host that ran cmd cleanly (so callers can
// print a per-host success line). Pass nil if no per-host feedback is
// needed.
//
// deploy_network.go::restartContainerdEverywhere is intentionally NOT
// rewritten on top of this helper — it iterates DPUs in addition to
// hosts, and aborting the goroutine-level structure for that case
// would lose clarity without saving much code.
func runOnHostsParallel(ctx context.Context, repo string, plan cluster.Plan, cmd string, perOK func(name string)) []string {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		failures []string
	)
	for name, h := range plan.HostByName {
		name, h := name, h
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg, err := sshConfigForHost(repo, h, 15*time.Second)
			if err != nil {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("%s: ssh cfg: %v", name, err))
				mu.Unlock()
				return
			}
			dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			c, err := ssh.Dial(dialCtx, cfg)
			cancel()
			if err != nil {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("%s: ssh dial: %v", name, err))
				mu.Unlock()
				return
			}
			defer c.Close()
			if r := c.Run(ctx, cmd); !r.OK() {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("%s: %s", name, strings.TrimSpace(r.Stderr+r.Stdout)))
				mu.Unlock()
				return
			}
			if perOK != nil {
				perOK(name)
			}
		}()
	}
	wg.Wait()
	return failures
}
