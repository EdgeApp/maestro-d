package devicefarm

import (
	"context"
	"sync"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
)

func TestProjectARNPassesThrough(t *testing.T) {
	c, f := newTestClient(t)
	arn := "arn:aws:devicefarm:us-west-2:123456789012:project:abc"
	c.project = arn
	got, err := c.Project(context.Background())
	if err != nil || got != arn {
		t.Fatalf("Project = %q, %v", got, err)
	}
	if len(f.Requests()) != 0 {
		t.Fatalf("an ARN made requests: %v", f.Ops())
	}
}

func TestProjectFoundByNameAcrossPages(t *testing.T) {
	c, f := newTestClient(t)
	f.SetPageSize(1)
	f.EnsureProject("a")
	f.EnsureProject("b")
	want := f.EnsureProject(DefaultProject)
	got, err := c.Project(context.Background())
	if err != nil || got != want {
		t.Fatalf("Project = %q, %v; want %q", got, err, want)
	}
	if f.CountOp("ListProjects") != 3 || f.CountOp("CreateProject") != 0 {
		t.Fatalf("ops = %v", f.Ops())
	}
	// Cached.
	if again, _ := c.Project(context.Background()); again != want || f.CountOp("ListProjects") != 3 {
		t.Fatalf("not cached: %v", f.Ops())
	}
}

func TestProjectCreatedOnce(t *testing.T) {
	c, f := newTestClient(t)
	var wg sync.WaitGroup
	arns := make([]string, 8)
	for i := range arns {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			arns[i], _ = c.Project(context.Background())
		}(i)
	}
	wg.Wait()
	for _, a := range arns {
		if a == "" || a != arns[0] {
			t.Fatalf("arns = %v", arns)
		}
	}
	if f.CountOp("CreateProject") != 1 {
		t.Fatalf("ops = %v", f.Ops())
	}
	if got := f.Projects(); len(got) != 1 || got[0] != DefaultProject {
		t.Fatalf("projects = %v", got)
	}
}

func TestProjectErrors(t *testing.T) {
	c, f := newTestClient(t)
	f.FailOp("ListProjects", "AccessDeniedException", "User: x is not authorized to perform: devicefarm:ListProjects on resource: y")
	_, err := c.Project(context.Background())
	if ce := Classify(err); ce.Kind != cloudprov.Auth || ce.Action != "devicefarm:ListProjects" {
		t.Fatalf("err = %#v", ce)
	}
	f.FailOp("ListProjects", "", "")
	f.FailOp("CreateProject", "LimitExceededException", "too many projects")
	if _, err := c.Project(context.Background()); Classify(err).Kind != cloudprov.NoCapacity {
		t.Fatalf("err = %v", err)
	}
}
