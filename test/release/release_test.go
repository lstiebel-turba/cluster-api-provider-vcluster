// Package release checks the assets hack/release.sh writes (make release): what clusterctl and
// the Cluster API Operator install.
package release

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

const (
	namespace = "capvc-system"
	// testImage is what the test builds with when RELEASE_DIR is not set.
	testImage = "ghcr.io/example/cluster-api-provider-vcluster:v0.4.0-test.1@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

var (
	components = []string{
		"infrastructure-components.yaml",
		"infrastructure-components-flux.yaml",
		"infrastructure-components-flux-namespaced.yaml",
	}
	// a digest, or a version tag (v<major>.<minor>.<patch>[-pre-release])
	pinnedImage = regexp.MustCompile(`^[^@\s]+(@sha256:[0-9a-f]{64}|:v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?)$`)
	variable    = regexp.MustCompile(`\$\{([^}]*)\}`)
)

func root(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func kustomize(t *testing.T) string {
	t.Helper()
	if path := filepath.Join(root(t), "bin", "kustomize"); fileExists(path) {
		return path
	}
	path, err := exec.LookPath("kustomize")
	if err != nil {
		t.Skip("no kustomize (make kustomize)")
	}
	return path
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func runRelease(t *testing.T, dir, version, image string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(filepath.Join(root(t), "hack", "release.sh"))
	cmd.Env = append(os.Environ(), "VERSION="+version, "IMAGE="+image, "RELEASE_DIR="+dir, "KUSTOMIZE="+kustomize(t))
	return cmd.CombinedOutput()
}

// releaseDir is RELEASE_DIR (make release sets it) or a fresh build with testImage.
func releaseDir(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("RELEASE_DIR"); dir != "" {
		return dir
	}
	dir := t.TempDir()
	if out, err := runRelease(t, dir, "v0.4.0-test.1", testImage); err != nil {
		t.Fatalf("hack/release.sh: %v\n%s", err, out)
	}
	return dir
}

func objects(t *testing.T, path string) []*unstructured.Unstructured {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var objs []*unstructured.Unstructured
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(&obj.Object); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("%s: %v", path, err)
		}
		if len(obj.Object) > 0 {
			objs = append(objs, obj)
		}
	}
	if len(objs) == 0 {
		t.Fatalf("%s: no objects", path)
	}
	return objs
}

func TestComponents(t *testing.T) {
	dir := releaseDir(t)
	for _, name := range components {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, []byte(":latest")) {
				t.Error("names a latest tag")
			}
			// clusterctl init needs no variable: each one has a default
			for _, m := range variable.FindAllStringSubmatch(string(data), -1) {
				if !strings.Contains(m[1], ":=") {
					t.Errorf("clusterctl variable %s has no default (${VAR:=default})", m[0])
				}
			}

			var crds, deployments int
			var args []string
			for _, obj := range objects(t, path) {
				kind, objName := obj.GetKind(), obj.GetName()
				switch kind {
				case "Namespace":
					if objName != namespace {
						t.Errorf("Namespace %s, want %s", objName, namespace)
					}
				case "CustomResourceDefinition":
					crds++
					if obj.GetLabels()["cluster.x-k8s.io/v1beta2"] == "" {
						t.Errorf("CRD %s has no cluster.x-k8s.io/v1beta2 contract label", objName)
					}
				case "Deployment":
					deployments++
					containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
					for _, c := range containers {
						image, _ := c.(map[string]any)["image"].(string)
						if !pinnedImage.MatchString(image) {
							t.Errorf("Deployment %s: image %q is pinned by neither digest nor version tag", objName, image)
						}
						a, _, _ := unstructured.NestedStringSlice(c.(map[string]any), "args")
						args = append(args, a...)
					}
				}
				if ns := obj.GetNamespace(); ns != "" && ns != namespace {
					t.Errorf("%s %s in namespace %s, want %s", kind, objName, ns, namespace)
				}
				if kind != "CustomResourceDefinition" && !strings.HasPrefix(objName, "capvc-") {
					t.Errorf("%s %s has no capvc- prefix", kind, objName)
				}
			}
			if crds == 0 || deployments != 1 {
				t.Errorf("%d CRDs and %d Deployments, want CRDs and one Deployment", crds, deployments)
			}
			flux := strings.Contains(name, "flux")
			if got := contains(args, "--installer=flux"); got != flux {
				t.Errorf("--installer=flux in args: %v, want %v (%v)", got, flux, args)
			}
			if got := bytes.Contains(data, []byte("capvc-helm-installer-role")); got == flux {
				t.Errorf("helm installer rights: %v, want %v", got, !flux)
			}
		})
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// metadata.yaml maps the release series to contract v1beta2 for this version.
func TestMetadata(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(releaseDir(t), "metadata.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		APIVersion    string `json:"apiVersion"`
		Kind          string `json:"kind"`
		ReleaseSeries []struct {
			Major, Minor int
			Contract     string
		} `json:"releaseSeries"`
	}
	if err := yaml.UnmarshalStrict(data, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.APIVersion != "clusterctl.cluster.x-k8s.io/v1alpha3" || len(metadata.ReleaseSeries) == 0 {
		t.Fatalf("not clusterctl metadata: %s", data)
	}
	if newest := metadata.ReleaseSeries[0]; newest.Contract != "v1beta2" {
		t.Errorf("newest release series %d.%d: contract %s, want v1beta2", newest.Major, newest.Minor, newest.Contract)
	}
}

// hack/release.sh refuses an unpinned image and a version without a release series.
func TestRefusals(t *testing.T) {
	for _, c := range []struct{ version, image string }{
		{"v0.4.0-test.1", "ghcr.io/example/cluster-api-provider-vcluster:latest"},
		{"v0.4.0-test.1", "ghcr.io/example/cluster-api-provider-vcluster"},
		{"v9.9.0", "ghcr.io/example/cluster-api-provider-vcluster:v9.9.0"},
		{"0.4.0", "ghcr.io/example/cluster-api-provider-vcluster:0.4.0"},
	} {
		if out, err := runRelease(t, t.TempDir(), c.version, c.image); err == nil {
			t.Errorf("VERSION=%s IMAGE=%s: want a refusal\n%s", c.version, c.image, out)
		}
	}
}
