package tools

import (
	"testing"
)

func TestUcloudSign(t *testing.T) {
	// UCloud signing: SHA1(sorted key-value concatenation + privateKey)
	params := map[string]string{
		"Action":    "DescribeCompShareInstance",
		"Region":    "cn-wlcb",
		"PublicKey": "testpubkey",
	}
	privateKey := "testprivkey"

	sig1 := ucloudSign(params, privateKey)
	if sig1 == "" {
		t.Fatal("ucloudSign returned empty string")
	}

	// Same input → same signature (deterministic)
	sig2 := ucloudSign(params, privateKey)
	if sig1 != sig2 {
		t.Errorf("ucloudSign not deterministic: %q != %q", sig1, sig2)
	}

	// Different privateKey → different signature
	sig3 := ucloudSign(params, "otherprivkey")
	if sig1 == sig3 {
		t.Error("different privateKey should produce different signature")
	}

	// Different params → different signature
	params2 := map[string]string{
		"Action":    "StartCompShareInstance",
		"Region":    "cn-wlcb",
		"PublicKey": "testpubkey",
	}
	sig4 := ucloudSign(params2, privateKey)
	if sig1 == sig4 {
		t.Error("different params should produce different signature")
	}
}

func TestUcloudSign_SortOrder(t *testing.T) {
	// Signature must be based on sorted keys, not insertion order
	params1 := map[string]string{"A": "1", "B": "2", "C": "3"}
	params2 := map[string]string{"C": "3", "A": "1", "B": "2"}

	sig1 := ucloudSign(params1, "key")
	sig2 := ucloudSign(params2, "key")
	if sig1 != sig2 {
		t.Errorf("signature should be order-independent: %q != %q", sig1, sig2)
	}
}

func TestFlattenInto_Simple(t *testing.T) {
	dst := make(map[string]string)
	src := map[string]any{
		"Zone":    "cn-wlcb-a",
		"GpuType": "4090",
		"Gpu":     1,
	}
	flattenInto(dst, src, "")

	if dst["Zone"] != "cn-wlcb-a" {
		t.Errorf("Zone = %q, want cn-wlcb-a", dst["Zone"])
	}
	if dst["GpuType"] != "4090" {
		t.Errorf("GpuType = %q, want 4090", dst["GpuType"])
	}
	if dst["Gpu"] != "1" {
		t.Errorf("Gpu = %q, want 1", dst["Gpu"])
	}
}

func TestFlattenInto_NestedMap(t *testing.T) {
	dst := make(map[string]string)
	src := map[string]any{
		"Config": map[string]any{
			"CPU":    16,
			"Memory": 65536,
		},
	}
	flattenInto(dst, src, "")

	if dst["Config.CPU"] != "16" {
		t.Errorf("Config.CPU = %q, want 16", dst["Config.CPU"])
	}
	if dst["Config.Memory"] != "65536" {
		t.Errorf("Config.Memory = %q, want 65536", dst["Config.Memory"])
	}
}

func TestFlattenInto_Array(t *testing.T) {
	dst := make(map[string]string)
	src := map[string]any{
		"Disks": []any{
			map[string]any{"IsBoot": true, "Size": 40},
			map[string]any{"IsBoot": false, "Size": 100},
		},
	}
	flattenInto(dst, src, "")

	if dst["Disks.0.IsBoot"] != "true" {
		t.Errorf("Disks.0.IsBoot = %q, want true", dst["Disks.0.IsBoot"])
	}
	if dst["Disks.0.Size"] != "40" {
		t.Errorf("Disks.0.Size = %q, want 40", dst["Disks.0.Size"])
	}
	if dst["Disks.1.Size"] != "100" {
		t.Errorf("Disks.1.Size = %q, want 100", dst["Disks.1.Size"])
	}
}

func TestFlattenInto_WithPrefix(t *testing.T) {
	dst := make(map[string]string)
	src := map[string]any{"Name": "test"}
	flattenInto(dst, src, "Prefix")

	if dst["Prefix.Name"] != "test" {
		t.Errorf("Prefix.Name = %q, want test", dst["Prefix.Name"])
	}
}
