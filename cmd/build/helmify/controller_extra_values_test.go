package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func TestStaticChartMCPServer(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{name: "default"},
		{
			name: "enabled",
			args: []string{"--set", "controller.mcpServer.enabled=true"},
			want: []string{"--mcp-server-enabled=true", "--mcp-max-concurrent=16"},
		},
		{
			name: "concurrency",
			args: []string{"--set", "controller.mcpServer.enabled=true", "--set", "controller.mcpServer.maxConcurrent=4"},
			want: []string{"--mcp-server-enabled=true", "--mcp-max-concurrent=4"},
		},
		{
			// The bound applies only to an enabled server.
			name: "disabled ignores concurrency",
			args: []string{"--set", "controller.mcpServer.maxConcurrent=0"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := renderedControllerDeployment(t, tc.args...).Spec.Template.Spec.Containers[0].Args
			var got []string
			for _, arg := range args {
				if strings.HasPrefix(arg, "--mcp-") {
					got = append(got, arg)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("MCP args = %q, want %q", got, tc.want)
			}
		})
	}

	// A string "false" is truthy in a template, so it must not turn the endpoint on.
	for _, value := range []string{"false", "true"} {
		t.Run("rejects enabled as the string "+value, func(t *testing.T) {
			output, err := helmTemplateStaticChart(t,
				"--set-string", "controller.mcpServer.enabled="+value,
				"--show-only", "templates/deployment.yaml")
			if err == nil || !strings.Contains(output, "controller.mcpServer.enabled must be true or false") {
				t.Fatalf("render with enabled=%q: %v\n%s", value, err, output)
			}
		})
	}

	for _, value := range []string{"0", "-1", "1.5", "many", "1000000"} {
		t.Run("rejects maxConcurrent "+value, func(t *testing.T) {
			output, err := helmTemplateStaticChart(t,
				"--set", "controller.mcpServer.enabled=true",
				"--set-string", "controller.mcpServer.maxConcurrent="+value,
				"--show-only", "templates/deployment.yaml")
			if err == nil {
				t.Fatalf("render succeeded with maxConcurrent=%s", value)
			}
			if !strings.Contains(output, "controller.mcpServer.maxConcurrent") {
				t.Fatalf("render failed without naming the value:\n%s", output)
			}
		})
	}
}

const extraVolumeValues = `
controller:
  extraVolumes:
    - name: snapshot-key-sync
      csi:
        driver: secrets-store.csi.k8s.io
        readOnly: true
        volumeAttributes:
          secretProviderClass: orka-snapshot-key
  extraVolumeMounts:
    - name: snapshot-key-sync
      mountPath: /mnt/secrets-store
      readOnly: true
`

func TestStaticChartControllerExtraVolumes(t *testing.T) {
	plain := renderedControllerDeployment(t).Spec.Template.Spec
	deployment := renderedControllerDeployment(t, "--values", writeStorageValues(t, extraVolumeValues))
	spec := deployment.Spec.Template.Spec
	controller := spec.Containers[0]

	plainMounts := plain.Containers[0].VolumeMounts
	if len(spec.Volumes) != len(plain.Volumes)+1 || len(controller.VolumeMounts) != len(plainMounts)+1 {
		t.Fatalf("want exactly one extra volume and mount after the chart's own, got %d volumes and %d mounts",
			len(spec.Volumes), len(controller.VolumeMounts))
	}
	if !slices.EqualFunc(spec.Volumes[:len(plain.Volumes)], plain.Volumes, volumesEqual) {
		t.Error("the chart's own volumes changed")
	}
	volume := spec.Volumes[len(spec.Volumes)-1]
	if volume.Name != "snapshot-key-sync" || volume.CSI == nil || volume.CSI.Driver != "secrets-store.csi.k8s.io" ||
		volume.CSI.VolumeAttributes["secretProviderClass"] != "orka-snapshot-key" {
		t.Errorf("extra volume = %+v", volume)
	}
	mount := controller.VolumeMounts[len(controller.VolumeMounts)-1]
	if mount.Name != "snapshot-key-sync" || mount.MountPath != "/mnt/secrets-store" || !mount.ReadOnly {
		t.Errorf("extra mount = %+v", mount)
	}

	for _, tc := range []struct {
		name, values, want string
	}{
		{
			name:   "reused chart volume name",
			values: "controller: {extraVolumes: [{name: store, emptyDir: {}}]}",
			want:   `controller.extraVolumes[0] reuses the chart's volume name "store"`,
		},
		{
			name:   "repeated name",
			values: "controller: {extraVolumes: [{name: extra, emptyDir: {}}, {name: extra, emptyDir: {}}]}",
			want:   `controller.extraVolumes[1] repeats the volume name "extra"`,
		},
		{
			name:   "volume without a name",
			values: "controller: {extraVolumes: [{emptyDir: {}}]}",
			want:   "controller.extraVolumes[0] must be a volume with a name",
		},
		{
			name:   "volumes not a list",
			values: "controller: {extraVolumes: {name: extra}}",
			want:   "controller.extraVolumes must be a list of volumes",
		},
		{
			name:   "mount of a chart volume",
			values: "controller: {extraVolumeMounts: [{name: store, mountPath: /mnt/store}]}",
			want:   `controller.extraVolumeMounts[0] must name a volume from controller.extraVolumes, not "store"`,
		},
		{
			name: "mount of an unknown volume",
			values: "controller: {extraVolumes: [{name: extra, emptyDir: {}}], " +
				"extraVolumeMounts: [{name: other, mountPath: /mnt/other}]}",
			want: `controller.extraVolumeMounts[0] must name a volume from controller.extraVolumes, not "other"`,
		},
		{
			name:   "mount at a chart path",
			values: extraMountValues("/var/run/orka/agent-execution-snapshot/"),
			want: "overlaps /var/run/orka/agent-execution-snapshot, " +
				`where the chart's "agent-execution-snapshot-key" volume is mounted`,
		},
		{
			name:   "mount inside a chart path",
			values: extraMountValues("/data/extra"),
			want:   `overlaps /data, where the chart's "store" volume is mounted`,
		},
		{
			name:   "mount above a chart path",
			values: extraMountValues("/var/run/orka/webhook"),
			want:   `overlaps /var/run/orka/webhook/tls, where the chart's "webhook-tls" volume is mounted`,
		},
		{
			name:   "mount through dot-dot",
			values: extraMountValues("/mnt/../data"),
			want:   `overlaps /data, where the chart's "store" volume is mounted`,
		},
		{
			name:   "mount over the ServiceAccount token",
			values: extraMountValues("/var/run/secrets/kubernetes.io/serviceaccount"),
			want: "overlaps /var/run/secrets/kubernetes.io/serviceaccount, " +
				"where the controller's ServiceAccount token is mounted",
		},
		{
			name: "repeated mount path",
			values: "controller: {extraVolumes: [{name: one, emptyDir: {}}, {name: two, emptyDir: {}}], " +
				"extraVolumeMounts: [{name: one, mountPath: /mnt/extra}, {name: two, mountPath: /mnt/extra/}]}",
			want: `controller.extraVolumeMounts[1].mountPath "/mnt/extra/" repeats another extra mount's path`,
		},
		{
			name:   "relative mount path",
			values: extraMountValues("mnt/extra"),
			want:   `controller.extraVolumeMounts[0].mountPath "mnt/extra" must be an absolute path other than /`,
		},
		{
			name:   "mount at the root",
			values: extraMountValues("/"),
			want:   `controller.extraVolumeMounts[0].mountPath "/" must be an absolute path other than /`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err := helmTemplateStaticChart(t, "--values", writeStorageValues(t, tc.values),
				"--show-only", "templates/deployment.yaml")
			if err == nil {
				t.Fatal("render succeeded")
			}
			if !strings.Contains(output, tc.want) {
				t.Fatalf("render failed without %q:\n%s", tc.want, output)
			}
		})
	}
}

// controllerTemplateMounts lists the name and mount path of every volume
// mount the controller container's template declares, whether or not any
// values render it.
func controllerTemplateMounts(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("static", "templates", "deployment.yaml"))
	if err != nil {
		t.Fatalf("read the Deployment template: %v", err)
	}
	template := string(data)
	start := strings.Index(template, "\n          volumeMounts:\n")
	end := strings.Index(template, "\n      volumes:\n")
	if start < 0 || end < start {
		t.Fatal("the controller container's volumeMounts block was not found")
	}
	block := template[start:end]
	pair := regexp.MustCompile(`(?m)^\s+- name: ([a-z0-9-]+)\n\s+mountPath: (/[^\s{}]*)$`)
	mounts := map[string]string{}
	for _, match := range pair.FindAllStringSubmatch(block, -1) {
		mounts[match[1]] = match[2]
	}
	// A mount whose name or path is templated would escape the pattern above.
	if declared := strings.Count(block, "mountPath:"); declared != len(mounts) {
		t.Fatalf("parsed %d of the %d mounts the template declares; give each a literal name and path", len(mounts), declared)
	}
	return mounts
}

// Every mount the controller template declares is reserved, with its path, so
// a mount added later fails this test until the reserved list covers it.
func TestStaticChartControllerExtraVolumesReserveEveryChartVolume(t *testing.T) {
	mounts := controllerTemplateMounts(t)
	for _, mount := range renderedControllerDeployment(t).Spec.Template.Spec.Containers[0].VolumeMounts {
		if mounts[mount.Name] != mount.MountPath {
			t.Fatalf("rendered mount %s at %s is not among the template's mounts %v", mount.Name, mount.MountPath, mounts)
		}
	}
	for name, path := range mounts {
		t.Run(name, func(t *testing.T) {
			volume := fmt.Sprintf("controller: {extraVolumes: [{name: %s, emptyDir: {}}]}", name)
			output, err := helmTemplateStaticChart(t, "--values", writeStorageValues(t, volume),
				"--show-only", "templates/deployment.yaml")
			if err == nil || !strings.Contains(output, "reuses the chart's volume name") {
				t.Errorf("volume name %q is not reserved: %v\n%s", name, err, output)
			}
			mount := extraMountValues(path)
			output, err = helmTemplateStaticChart(t, "--values", writeStorageValues(t, mount),
				"--show-only", "templates/deployment.yaml")
			want := fmt.Sprintf("where the chart's %q volume is mounted", name)
			if err == nil || !strings.Contains(output, want) {
				t.Errorf("mount path %s of %q is not reserved: %v\n%s", path, name, err, output)
			}
		})
	}
}

// extraMountValues mounts one extra emptyDir volume, extra, at path.
func extraMountValues(path string) string {
	return fmt.Sprintf("controller: {extraVolumes: [{name: extra, emptyDir: {}}], "+
		"extraVolumeMounts: [{name: extra, mountPath: %q}]}", path)
}

func volumesEqual(a, b corev1.Volume) bool {
	left, err := yaml.Marshal(a)
	if err != nil {
		return false
	}
	right, err := yaml.Marshal(b)
	return err == nil && string(left) == string(right)
}
