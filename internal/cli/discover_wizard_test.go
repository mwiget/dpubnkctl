package cli

import (
	"strings"
	"testing"
)

func TestSuggestRole(t *testing.T) {
	cases := []struct {
		name           string
		dpuCount       int
		totalReachable int
		noDPUHosts     int
		wantRole       string
		wantInRation   string
	}{
		{
			name:           "no-DPU host suggested CP",
			dpuCount:       0,
			totalReachable: 3,
			noDPUHosts:     1,
			wantRole:       "control-plane",
			wantInRation:   "no DPU",
		},
		{
			name:           "DPU host with 3 CP-free siblings → worker",
			dpuCount:       1,
			totalReachable: 5,
			noDPUHosts:     3,
			wantRole:       "worker",
			wantInRation:   "DPU-free hosts available as dedicated control planes",
		},
		{
			name:           "DPU host with 4 CP-free siblings → worker",
			dpuCount:       2,
			totalReachable: 6,
			noDPUHosts:     4,
			wantRole:       "worker",
			wantInRation:   "this host is worker only",
		},
		{
			name:           "single-host lab (1 DPU host, 0 CP-free) → both",
			dpuCount:       1,
			totalReachable: 1,
			noDPUHosts:     0,
			wantRole:       "both",
			wantInRation:   "single-host lab",
		},
		{
			name:           "small lab (2 DPU hosts, 0 CP-free) → both",
			dpuCount:       1,
			totalReachable: 2,
			noDPUHosts:     0,
			wantRole:       "both",
			wantInRation:   "runs control plane and worker",
		},
		{
			name:           "4 hosts mixed (2 DPU + 2 CP-free) → both for DPU host",
			dpuCount:       1,
			totalReachable: 4,
			noDPUHosts:     2,
			wantRole:       "both",
			wantInRation:   "fewer than the 3 control-plane-only nodes etcd needs for HA",
		},
		{
			name:           "4 hosts mixed (2 DPU + 2 CP-free) → control-plane for DPU-free host",
			dpuCount:       0,
			totalReachable: 4,
			noDPUHosts:     2,
			wantRole:       "control-plane",
			wantInRation:   "no DPU",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			role, rationale := suggestRole(c.dpuCount, c.totalReachable, c.noDPUHosts)
			if role != c.wantRole {
				t.Errorf("role = %q, want %q", role, c.wantRole)
			}
			if !strings.Contains(rationale, c.wantInRation) {
				t.Errorf("rationale = %q, want substring %q", rationale, c.wantInRation)
			}
		})
	}
}
