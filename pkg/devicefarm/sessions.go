package devicefarm

import (
	"context"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/devicefarm"
	"github.com/aws/aws-sdk-go-v2/service/devicefarm/types"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
)

// Session timing; tests shorten them.
var (
	sessionPollInterval  = 3 * time.Second
	pendingDeviceTimeout = 120 * time.Second
)

// Remote access session states.
const (
	StatePending            = "PENDING"
	StatePendingConcurrency = "PENDING_CONCURRENCY"
	StatePendingDevice      = "PENDING_DEVICE"
	StateRunning            = "RUNNING"
	StateStopping           = "STOPPING"
	StateCompleted          = "COMPLETED"
)

// SessionSpec is what Create asks Device Farm for.
type SessionSpec struct {
	DeviceARN     string
	Platform      string // android | ios
	OSVersion     string // the device's OS, for the Appium version default
	AppARN        string // optional
	AuxiliaryApps []string
	AppiumVersion string // "2" | "3"; empty picks by platform and OS
}

// Session is a remote access session. Endpoint is a credential.
type Session struct {
	ARN        string
	Name       string
	Status     string
	Result     string
	Message    string
	Endpoint   string // endpoints.remoteDriverEndpoint, once RUNNING
	DeviceARN  string
	DeviceName string
	Created    time.Time
	Started    time.Time
	Minutes    *cloudprov.DeviceMinutes
}

// AppiumVersion is the Appium major version a session gets: "3" for iOS 27
// and later, which requires it, else "2".
func AppiumVersion(platform, osVersion string) string {
	if strings.EqualFold(platform, "ios") {
		if v, ok := cloudprov.ParseVersion(osVersion); ok && v[0] >= 27 {
			return "3"
		}
	}
	return "2"
}

// Create starts a metered remote access session named name.
func (c *Client) Create(ctx context.Context, spec SessionSpec, name string) (string, error) {
	project, err := c.Project(ctx)
	if err != nil {
		return "", err
	}
	version := spec.AppiumVersion
	if version == "" {
		version = AppiumVersion(spec.Platform, spec.OSVersion)
	}
	in := &devicefarm.CreateRemoteAccessSessionInput{
		ProjectArn: aws.String(project),
		DeviceArn:  aws.String(spec.DeviceARN),
		Configuration: &types.CreateRemoteAccessSessionConfiguration{
			BillingMethod: types.BillingMethodMetered,
			AuxiliaryApps: spec.AuxiliaryApps,
			Parameters:    map[string]string{"appium:version": version},
		},
	}
	if name != "" {
		in.Name = aws.String(name)
	}
	if spec.AppARN != "" {
		in.AppArn = aws.String(spec.AppARN)
	}
	out, err := c.api.CreateRemoteAccessSession(ctx, in)
	if err != nil {
		return "", err
	}
	if out.RemoteAccessSession == nil || str(out.RemoteAccessSession.Arn) == "" {
		return "", cloudprov.Errorf(cloudprov.Other, "Device Farm CreateRemoteAccessSession returned no session")
	}
	return str(out.RemoteAccessSession.Arn), nil
}

// Get reads a session.
func (c *Client) Get(ctx context.Context, arn string) (*Session, error) {
	out, err := c.api.GetRemoteAccessSession(ctx, &devicefarm.GetRemoteAccessSessionInput{Arn: aws.String(arn)})
	if err != nil {
		return nil, err
	}
	if out.RemoteAccessSession == nil {
		return nil, cloudprov.Errorf(cloudprov.Other, "Device Farm GetRemoteAccessSession returned no session")
	}
	return toSession(*out.RemoteAccessSession), nil
}

// Wait polls a session until it is RUNNING and returns it with its endpoint.
//
//   - STOPPING or COMPLETED first is an Ended error.
//   - PENDING_DEVICE held continuously for pendingDeviceTimeout is a
//     DeviceBusy error.
//   - Not RUNNING within timeout is a NoCapacity error when the session was
//     waiting in PENDING_CONCURRENCY, else Other.
//
// progress, when set, is called with each new state.
func (c *Client) Wait(ctx context.Context, arn string, timeout time.Duration, progress func(string)) (*Session, error) {
	start := time.Now()
	var last string
	var pendingDeviceSince time.Time
	for {
		s, err := c.Get(ctx, arn)
		if err != nil {
			return nil, err
		}
		if s.Status != last {
			last = s.Status
			if progress != nil {
				progress(s.Status)
			}
			if s.Status == StatePendingDevice {
				pendingDeviceSince = time.Now()
			}
		}
		switch s.Status {
		case StateRunning:
			if s.Endpoint == "" {
				return nil, cloudprov.Errorf(cloudprov.Other, "Device Farm session is RUNNING without an Appium endpoint")
			}
			return s, nil
		case StateStopping, StateCompleted:
			return nil, cloudprov.Errorf(cloudprov.Ended, "Device Farm ended the session before it ran (%s)", s.endedReason())
		case StatePendingDevice:
			if time.Since(pendingDeviceSince) >= pendingDeviceTimeout {
				return nil, cloudprov.Errorf(cloudprov.DeviceBusy,
					"the Device Farm device stayed busy (PENDING_DEVICE) for %s", pendingDeviceTimeout)
			}
		}
		if time.Since(start) >= timeout {
			if s.Status == StatePendingConcurrency {
				return nil, cloudprov.Errorf(cloudprov.NoCapacity,
					"the Device Farm session waited %s in PENDING_CONCURRENCY: Device Farm allows 2 metered remote-access "+
						"sessions at once across the whole account by default (Service Quotas L-F73D98D4); "+
						"release another cloud device or raise the quota", timeout)
			}
			return nil, cloudprov.Errorf(cloudprov.Other, "the Device Farm session did not start within %s (last state %s)", timeout, s.Status)
		}
		if err := sleep(ctx, sessionPollInterval); err != nil {
			return nil, err
		}
	}
}

// endedReason describes why a session ended.
func (s *Session) endedReason() string {
	parts := []string{s.Status}
	if s.Result != "" {
		parts = append(parts, s.Result)
	}
	if s.Message != "" {
		parts = append(parts, s.Message)
	}
	return Redact(strings.Join(parts, ": "))
}

// Stop stops a session and reports its device minutes, when Device Farm
// already knows them.
func (c *Client) Stop(ctx context.Context, arn string) (*cloudprov.DeviceMinutes, error) {
	out, err := c.api.StopRemoteAccessSession(ctx, &devicefarm.StopRemoteAccessSessionInput{Arn: aws.String(arn)})
	if err != nil {
		return nil, err
	}
	if out.RemoteAccessSession != nil {
		return minutes(out.RemoteAccessSession.DeviceMinutes), nil
	}
	return nil, nil
}

// Install installs an uploaded app into a running session.
func (c *Client) Install(ctx context.Context, arn, appARN string) (*cloudprov.AppRef, error) {
	out, err := c.api.InstallToRemoteAccessSession(ctx, &devicefarm.InstallToRemoteAccessSessionInput{
		RemoteAccessSessionArn: aws.String(arn),
		AppArn:                 aws.String(appARN),
	})
	if err != nil {
		return nil, err
	}
	ref := cloudprov.AppRef{Ref: appARN}
	if out.AppUpload != nil {
		ref = appRef(*out.AppUpload)
		if ref.Ref == "" {
			ref.Ref = appARN
		}
	}
	return &ref, nil
}

// List lists the project's remote access sessions.
func (c *Client) List(ctx context.Context) ([]Session, error) {
	project, err := c.Project(ctx)
	if err != nil {
		return nil, err
	}
	var all []Session
	in := &devicefarm.ListRemoteAccessSessionsInput{Arn: aws.String(project)}
	for {
		out, err := c.api.ListRemoteAccessSessions(ctx, in)
		if err != nil {
			return nil, err
		}
		for _, s := range out.RemoteAccessSessions {
			all = append(all, *toSession(s))
		}
		if str(out.NextToken) == "" {
			return all, nil
		}
		in.NextToken = out.NextToken
	}
}

func toSession(s types.RemoteAccessSession) *Session {
	out := &Session{
		ARN:     str(s.Arn),
		Name:    str(s.Name),
		Status:  string(s.Status),
		Result:  string(s.Result),
		Message: str(s.Message),
		Minutes: minutes(s.DeviceMinutes),
	}
	if s.Endpoints != nil {
		out.Endpoint = str(s.Endpoints.RemoteDriverEndpoint)
	}
	if s.Device != nil {
		out.DeviceARN = str(s.Device.Arn)
		out.DeviceName = str(s.Device.Name)
	}
	if s.Created != nil {
		out.Created = *s.Created
	}
	if s.Started != nil {
		out.Started = *s.Started
	}
	return out
}

func minutes(m *types.DeviceMinutes) *cloudprov.DeviceMinutes {
	if m == nil {
		return nil
	}
	return &cloudprov.DeviceMinutes{Metered: aws.ToFloat64(m.Metered), Total: aws.ToFloat64(m.Total)}
}
