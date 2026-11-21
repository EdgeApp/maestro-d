package devicefarm

import (
	"context"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/devicefarm"
)

// Project returns the ARN of the configured project. An ARN passes through;
// a name is looked up and created once when missing. The answer is cached.
func (c *Client) Project(ctx context.Context) (string, error) {
	if strings.HasPrefix(c.project, "arn:") {
		return c.project, nil
	}
	c.projectMu.Lock()
	defer c.projectMu.Unlock()
	if c.projectARN != "" {
		return c.projectARN, nil
	}
	pages := devicefarm.NewListProjectsPaginator(c.api, &devicefarm.ListProjectsInput{})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return "", err
		}
		for _, p := range page.Projects {
			if str(p.Name) == c.project && str(p.Arn) != "" {
				c.projectARN = str(p.Arn)
				return c.projectARN, nil
			}
		}
	}
	out, err := c.api.CreateProject(ctx, &devicefarm.CreateProjectInput{Name: aws.String(c.project)})
	if err != nil {
		return "", err
	}
	if out.Project == nil || str(out.Project.Arn) == "" {
		return "", errors.New("Device Farm CreateProject returned no project")
	}
	c.projectARN = str(out.Project.Arn)
	return c.projectARN, nil
}
