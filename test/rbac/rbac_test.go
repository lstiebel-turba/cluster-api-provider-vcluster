// Package rbac checks the provider's RBAC manifests in config/.
package rbac

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

const configDir = "../../config"

// grant is one verb on one resource of one API group.
type grant struct{ group, resource, verb string }

func readClusterRole(t *testing.T, path string) *rbacv1.ClusterRole {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(configDir, path))
	if err != nil {
		t.Fatal(err)
	}
	role := &rbacv1.ClusterRole{}
	if err := yaml.Unmarshal(data, role); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if role.Kind != "ClusterRole" || len(role.Rules) == 0 {
		t.Fatalf("%s: not a ClusterRole with rules", path)
	}
	return role
}

func grants(rules []rbacv1.PolicyRule) map[grant]bool {
	set := map[grant]bool{}
	for _, rule := range rules {
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				for _, verb := range rule.Verbs {
					set[grant{group, resource, verb}] = true
				}
			}
		}
	}
	return set
}

func noWildcards(t *testing.T, name string, rules []rbacv1.PolicyRule) {
	t.Helper()
	for _, rule := range rules {
		for _, value := range append(append(append([]string{}, rule.APIGroups...), rule.Resources...), rule.Verbs...) {
			if strings.Contains(value, "*") {
				t.Errorf("%s: wildcard %q in %+v", name, value, rule)
			}
		}
		if len(rule.NonResourceURLs) > 0 {
			t.Errorf("%s: non-resource URLs %v", name, rule.NonResourceURLs)
		}
	}
}

// The provider's own role (generated from the +kubebuilder:rbac markers) holds no wildcard, no
// chart rights and reads no Secret by list or watch.
func TestManagerRole(t *testing.T) {
	role := readClusterRole(t, "rbac/role.yaml")
	noWildcards(t, "manager-role", role.Rules)
	for g := range grants(role.Rules) {
		switch {
		case g.group != "" && !strings.HasSuffix(g.group, ".x-k8s.io") && !strings.HasSuffix(g.group, ".fluxcd.io"):
			t.Errorf("manager-role: chart rights %v belong in rbac/helm-installer", g)
		case g.group == "" && g.resource == "secrets" && (g.verb == "list" || g.verb == "watch"):
			t.Errorf("manager-role: %v reads every Secret", g)
		}
	}
}

// The namespaced component splits manager-role without adding or dropping a right: cluster-wide
// what has no namespace or is watched, per namespace the rest.
func TestNamespacedSplit(t *testing.T) {
	want := grants(readClusterRole(t, "rbac/role.yaml").Rules)
	clusterRole := readClusterRole(t, "rbac/namespaced/cluster_role.yaml")
	namespaceRole := readClusterRole(t, "rbac/namespaced/namespace_role.yaml")
	noWildcards(t, "manager-cluster-role", clusterRole.Rules)
	noWildcards(t, "manager-namespace-role", namespaceRole.Rules)

	namespaced := map[string]bool{
		"secrets": true, "services": true, "persistentvolumeclaims": true,
		"helmreleases": true, "helmrepositories": true,
	}
	// the cache watches HelmReleases (metadata) wherever it watches VClusters: cluster-wide
	perNamespace := func(g grant) bool {
		return namespaced[g.resource] && (g.resource != "helmreleases" || (g.verb != "list" && g.verb != "watch"))
	}
	got := map[grant]bool{}
	for g := range grants(clusterRole.Rules) {
		if perNamespace(g) {
			t.Errorf("manager-cluster-role: %v belongs in manager-namespace-role", g)
		}
		got[g] = true
	}
	for g := range grants(namespaceRole.Rules) {
		if !perNamespace(g) {
			t.Errorf("manager-namespace-role: %v belongs in manager-cluster-role", g)
		}
		got[g] = true
	}
	for g := range want {
		if !got[g] {
			t.Errorf("namespaced component misses %v of manager-role", g)
		}
	}
	for g := range got {
		if !want[g] {
			t.Errorf("namespaced component grants %v, manager-role does not", g)
		}
	}
}

// The helm installer's role is broad but names what it grants.
func TestHelmInstallerRole(t *testing.T) {
	noWildcards(t, "helm-installer-role", readClusterRole(t, "rbac/helm-installer/helm_installer_role.yaml").Rules)
}

// No binding in config/ refers to cluster-admin.
func TestNoClusterAdmin(t *testing.T) {
	var found []string
	err := filepath.WalkDir(configDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, doc := range strings.Split(string(data), "\n---") {
			binding := &rbacv1.ClusterRoleBinding{}
			if err := yaml.Unmarshal([]byte(doc), binding); err != nil {
				continue // not every document parses as a binding (patches, lists)
			}
			if strings.HasSuffix(binding.Kind, "RoleBinding") && binding.RoleRef.Name == "cluster-admin" {
				found = append(found, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(found)
	if len(found) > 0 {
		t.Errorf("bindings to cluster-admin: %v", found)
	}
}
