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
		"vm", "run",
		"--name", "vm-linux",
		"--cpu", "2",
		"--memory", "8G",
		"--storage", "100G",
		"--nics", "1",
		"--dns", "8.8.8.8,1.1.1.1",
		"--default-root-password", "rootpass",
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
		"vm", "run",
		"--name", "vm-win",
		"--cpu", "4",
		"--memory", "8G",
		"--storage", "100G",
		"--nics", "1",
		"--windows",
		"--dns", "10.8.8.8,10.8.8.9",
		"win1125h2",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildRunArgs(windows) mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestBuildCloneArgs(t *testing.T) {
	got := buildCloneArgs("vm-clone", "2", "8G", "40G", "snapshot-ref")
	want := []string{
		"vm", "clone", "--cold",
		"--name", "vm-clone",
		"--cpu", "2",
		"--memory", "8G",
		"--storage", "40G",
		"snapshot-ref",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildCloneArgs mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}
