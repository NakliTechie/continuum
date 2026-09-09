package workspace

import "testing"

func TestAuditWorkspaceReusedForDifferentRepo(t *testing.T) {
	a, b := testRepo(t), testRepo(t)
	p := New(t.TempDir())
	spec := specWithPorts(4400, 4499)
	spec.Workspace.Materialise.Ports = nil
	if _, err := p.Provision(spec, a, "w1"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Provision(spec, b, "w1"); err == nil {
		t.Fatal("reassigned workspace to another repository")
	}
	rec, err := p.Load("w1")
	if err != nil || rec.Repo != a {
		t.Fatal("existing identity changed", err)
	}
}
