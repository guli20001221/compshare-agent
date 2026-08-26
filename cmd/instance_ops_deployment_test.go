package main

import (
	"bytes"
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestProductionAgentHomeIsWritableByNonRootSSHOpsRuntime(t *testing.T) {
	raw, err := os.ReadFile("../deploy/k8s/deployment.yaml")
	if err != nil {
		t.Fatalf("read production deployment: %v", err)
	}
	var deployment struct {
		Spec struct {
			Template struct {
				Spec struct {
					SecurityContext struct {
						FSGroup int64 `yaml:"fsGroup"`
					} `yaml:"securityContext"`
					Containers []struct {
						Name            string `yaml:"name"`
						SecurityContext struct {
							RunAsUser int64 `yaml:"runAsUser"`
						} `yaml:"securityContext"`
						VolumeMounts []struct {
							Name      string `yaml:"name"`
							MountPath string `yaml:"mountPath"`
						} `yaml:"volumeMounts"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.NewDecoder(bytes.NewReader(raw)).Decode(&deployment); err != nil {
		t.Fatalf("decode production deployment: %v", err)
	}
	pod := deployment.Spec.Template.Spec
	if pod.SecurityContext.FSGroup != 10001 {
		t.Fatalf("agent-home emptyDir must be writable by gid 10001, got fsGroup=%d", pod.SecurityContext.FSGroup)
	}
	for _, container := range pod.Containers {
		if container.Name != "compshare-agent" {
			continue
		}
		if container.SecurityContext.RunAsUser != 10001 {
			t.Fatalf("compshare-agent runAsUser=%d, want 10001", container.SecurityContext.RunAsUser)
		}
		for _, mount := range container.VolumeMounts {
			if mount.Name == "agent-home" && mount.MountPath == "/home/compshare" {
				return
			}
		}
		t.Fatal("compshare-agent no longer mounts agent-home at /home/compshare")
	}
	t.Fatal("production deployment has no compshare-agent container")
}
