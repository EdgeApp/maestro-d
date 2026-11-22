// Package devicefarm is the AWS Device Farm cloud provider: a client for the
// Device Farm API (projects, the device catalog, app uploads, remote access
// sessions and account settings) and a cloudprov.RemoteProvider built on it.
//
// Device Farm lives in us-west-2 only. Credentials come from the standard AWS
// chain of the process, or from an explicit credentials file or profile. The
// session endpoint URL is itself a credential, so everything this package
// reports passes through Redact first.
package devicefarm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/devicefarm"
)

// Region is the only region Device Farm runs in.
const Region = "us-west-2"

// DefaultProject is the project name used when none is configured.
const DefaultProject = "maestro-d"

// Environment variables the client reads.
const (
	// EnvEndpoint overrides the Device Farm API endpoint. It is for tests,
	// which point it at a dftest fake.
	EnvEndpoint = "MAESTRO_D_DEVICEFARM_ENDPOINT"
	// EnvProject names the project (a name or an ARN) when Options.Project
	// is empty.
	EnvProject = "MAESTRO_D_DEVICEFARM_PROJECT"
)

// ErrNoCredentials means no AWS credentials resolved for Device Farm.
var ErrNoCredentials = errors.New("no AWS credentials for Device Farm: set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, " +
	"or AWS_PROFILE with a profile in ~/.aws/credentials or ~/.aws/config. " +
	"The maestro-d daemon reads them from its own environment, so after setting them " +
	"run `maestro-d stop` and run the command again from a shell that has them")

// Options configures a Client.
type Options struct {
	// CredentialsFile is a shared credentials file to read keys from. It
	// takes precedence over credentials in the environment.
	CredentialsFile string
	// Profile is the shared config profile to use.
	Profile string
	// Project is a project name or ARN; empty means MAESTRO_D_DEVICEFARM_PROJECT,
	// then "maestro-d".
	Project string
	// Endpoint overrides the API endpoint, ahead of MAESTRO_D_DEVICEFARM_ENDPOINT.
	// It is for tests.
	Endpoint string
}

// Client talks to the Device Farm API for one project.
type Client struct {
	api      *devicefarm.Client
	http     *http.Client
	redactor *Redactor
	project  string // the configured name or ARN

	projectMu  sync.Mutex
	projectARN string

	catalogMu sync.Mutex
	catalog   map[string]catalogEntry

	now func() time.Time
}

// New loads the AWS configuration, resolves credentials once (so a daemon
// without credentials fails fast) and returns a client.
func New(ctx context.Context, opts Options) (*Client, error) {
	loadOpts := []func(*config.LoadOptions) error{config.WithRegion(Region)}
	if opts.CredentialsFile != "" {
		loadOpts = append(loadOpts, config.WithSharedCredentialsFiles([]string{opts.CredentialsFile}))
		if opts.Profile == "" {
			// Naming the profile makes the file win over keys in the
			// environment, which the default chain would otherwise prefer.
			loadOpts = append(loadOpts, config.WithSharedConfigProfile("default"))
		}
	}
	if opts.Profile != "" {
		loadOpts = append(loadOpts, config.WithSharedConfigProfile(opts.Profile))
	}
	endpoint := opts.Endpoint
	if endpoint == "" {
		endpoint = os.Getenv(EnvEndpoint)
	}
	if endpoint != "" {
		// A test endpoint never needs retries; they only slow failures down.
		loadOpts = append(loadOpts, config.WithBaseEndpoint(endpoint), config.WithRetryMaxAttempts(1))
	}
	cfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, noCredentials(err)
	}
	if cfg.Credentials == nil {
		return nil, ErrNoCredentials
	}
	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return nil, noCredentials(err)
	}
	if !creds.HasKeys() {
		return nil, ErrNoCredentials
	}
	project := opts.Project
	if project == "" {
		project = os.Getenv(EnvProject)
	}
	if project == "" {
		project = DefaultProject
	}
	return &Client{
		api:      devicefarm.NewFromConfig(cfg),
		http:     &http.Client{},
		redactor: NewRedactor(creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken),
		project:  project,
		catalog:  map[string]catalogEntry{},
		now:      time.Now,
	}, nil
}

// noCredentials wraps a credential resolution failure as ErrNoCredentials,
// keeping the cause (redacted) for the operator.
func noCredentials(cause error) error {
	return fmt.Errorf("%w (%s)", ErrNoCredentials, Redact(cause.Error()))
}

// Redactor returns the client's redactor, which also knows the loaded keys.
func (c *Client) Redactor() *Redactor { return c.redactor }

// str returns *p, or "" for nil.
func str(p *string) string { return aws.ToString(p) }
