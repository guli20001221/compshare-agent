package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/tools"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: external_control <config> <describe|start|stop> <uhost-id> [zone]")
	os.Exit(2)
}

func waitForStable(ctx context.Context, ex *tools.ExternalExecutor, id string) (string, error) {
	for i := 0; i < 36; i++ {
		state, err := describeState(ctx, ex, id)
		if err != nil {
			return "", err
		}
		fmt.Printf("state=%s\n", state)
		switch state {
		case "Running", "Stopped", "Install Fail":
			return state, nil
		}
		time.Sleep(5 * time.Second)
	}
	return "", fmt.Errorf("timed out waiting for stable state")
}

func describeState(ctx context.Context, ex *tools.ExternalExecutor, id string) (string, error) {
	res, err := ex.Execute(ctx, "DescribeCompShareInstance", map[string]any{
		"UHostIds": []any{id},
		"Limit":    1,
		"Offset":   0,
	})
	if err != nil {
		return "", err
	}
	set, _ := res["UHostSet"].([]any)
	if len(set) == 0 {
		return "", fmt.Errorf("instance not found")
	}
	item, ok := set[0].(map[string]any)
	if !ok {
		return "", fmt.Errorf("unexpected instance payload")
	}
	return fmt.Sprint(item["State"]), nil
}

func main() {
	if len(os.Args) < 4 {
		usage()
	}
	cfgPath := os.Args[1]
	action := os.Args[2]
	id := os.Args[3]
	zone := "cn-wlcb-01"
	if len(os.Args) >= 5 {
		zone = os.Args[4]
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		panic(err)
	}
	// PR9: ExternalExecutor.SetProjectId / ProjectId() removed (cross-session
	// leak fix). For this eval harness we inject ProjectId at cfg time before
	// constructing the executor instead.
	if cfg.Agent.ProjectId == "" {
		cfg.Agent.ProjectId = "org-cwy2qk"
	}
	ex := tools.NewExternalExecutor(cfg.Agent)

	ctx := context.Background()
	switch action {
	case "describe":
		state, err := describeState(ctx, ex, id)
		if err != nil {
			panic(err)
		}
		fmt.Println(state)
	case "stop":
		args := map[string]any{
			"UHostId": id,
			"Zone":    zone,
		}
		for i := 0; i < 6; i++ {
			if _, err := ex.Execute(ctx, "StopCompShareInstance", args); err != nil {
				if strings.Contains(err.Error(), "RetCode=8903") {
					if _, waitErr := waitForStable(ctx, ex, id); waitErr != nil {
						panic(waitErr)
					}
					time.Sleep(3 * time.Second)
					continue
				}
				panic(err)
			}
			finalState, err := waitForStable(ctx, ex, id)
			if err != nil {
				panic(err)
			}
			fmt.Printf("final=%s\n", finalState)
			return
		}
		panic("stop retries exhausted")
	case "start":
		args := map[string]any{
			"UHostId": id,
			"Zone":    zone,
		}
		for i := 0; i < 6; i++ {
			if _, err := ex.Execute(ctx, "StartCompShareInstance", args); err != nil {
				if strings.Contains(err.Error(), "RetCode=8903") {
					if _, waitErr := waitForStable(ctx, ex, id); waitErr != nil {
						panic(waitErr)
					}
					time.Sleep(3 * time.Second)
					continue
				}
				panic(err)
			}
			finalState, err := waitForStable(ctx, ex, id)
			if err != nil {
				panic(err)
			}
			fmt.Printf("final=%s\n", finalState)
			return
		}
		panic("start retries exhausted")
	default:
		usage()
	}
}
