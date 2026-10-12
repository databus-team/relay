package main

import (
	"reflect"
	"testing"
)

// runningEgressIDs 决定 `relay ps` 要不要发那次中转探测、以及只探哪些出口:
// 只有运行中的隧道才有出口可探,且同一出口去重。
func TestRunningEgressIDs(t *testing.T) {
	cases := []struct {
		name    string
		tunnels []tunnelState
		want    []string
	}{
		{
			name:    "没有隧道",
			tunnels: nil,
			want:    nil,
		},
		{
			name: "只收运行中的隧道",
			tunnels: []tunnelState{
				{Egress: "eg-a", State: "running"},
				{Egress: "eg-b", State: "stopped"},
			},
			want: []string{"eg-a"},
		},
		{
			name: "同一出口去重",
			tunnels: []tunnelState{
				{Egress: "eg-a", State: "running"},
				{Egress: "eg-a", State: "running"},
				{Egress: "eg-b", State: "running"},
			},
			want: []string{"eg-a", "eg-b"},
		},
		{
			name: "出口为空不算一个待探对象",
			tunnels: []tunnelState{
				{Egress: "", State: "running"},
				{Egress: "eg-a", State: "running"},
			},
			want: []string{"eg-a"},
		},
		{
			name: "全部停止则一次探测也不发",
			tunnels: []tunnelState{
				{Egress: "eg-a", State: "stopped"},
				{Egress: "eg-b", State: "stopped"},
			},
			want: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := runningEgressIDs(c.tunnels); !reflect.DeepEqual(got, c.want) {
				t.Errorf("runningEgressIDs = %v, want %v", got, c.want)
			}
		})
	}
}
