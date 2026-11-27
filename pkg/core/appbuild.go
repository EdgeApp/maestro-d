package core

// maestro-d: where a driver finds the build of an app it must reinstall.

// AppBuildSource answers the build last installed for appID on the
// driver's device: a path on the driver's host, or "" when none is kept.
type AppBuildSource func(appID string) string

// AppBuildUser is a driver whose clearState reinstalls the app from its
// build (WebDriverAgent on a physical iPhone or iPad, where nothing else
// clears an app's data). The daemon gives it the builds installApp keeps.
type AppBuildUser interface {
	SetAppBuildSource(AppBuildSource)
}
