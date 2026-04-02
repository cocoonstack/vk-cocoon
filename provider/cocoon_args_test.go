package provider

import (
	"reflect"
	"testing"
)

func TestBuildRunArgsLinux(t *testing.T) {
	got := buildRunArgs(runConfig{
		vmName: "vm-linux", cpu: "2", mem: "8G", storage: "100G",
		nics: "1", dns: "8.8.8.8,1.1.1.1", rootPwd: "rootpass",
		image: "ubuntu.img", osType: "linux",
	})
	want := []string{
		"run",
		"--name", "vm-linux",
		"--cpus", "2",
		"--memory", "8G",
		"--disk", "100G",
		"ubuntu.img",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildRunArgs(linux) mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestBuildRunArgsWindows(t *testing.T) {
	got := buildRunArgs(runConfig{
		vmName: "vm-win", cpu: "4", mem: "8G", storage: "100G",
		nics: "1", dns: "10.8.8.8,10.8.8.9", rootPwd: "ignored",
		image: "win1125h2", osType: "windows",
	})
	want := []string{
		"run",
		"--name", "vm-win",
		"--cpus", "4",
		"--memory", "8G",
		"--disk", "100G",
		"win1125h2",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildRunArgs(windows) mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestBuildCloneArgs(t *testing.T) {
	got := buildCloneArgs("vm-clone", "snapshot-ref")
	want := []string{
		"vm", "clone",
		"--name", "vm-clone",
		"snapshot-ref",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildCloneArgs mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}
