# iOS screen lock and unlock

The native WDA driver supports explicit `LOCK` and `UNLOCK` values for
`pressKey` (case-insensitive). These are idempotent screen-state commands,
not a power-button toggle. Each command verifies the resulting lock state;
a request that leaves the screen in the wrong state fails.

```yaml
- pressKey: LOCK
# Run the background/reconnection observation here.
- pressKey: UNLOCK
```

With an already attached device:

```sh
maestro-d pressKey LOCK --daemon ios-test
maestro-d pressKey UNLOCK --daemon ios-test
```

The same commands are available through the daemon API and TypeScript client:

```ts
await device.pressKey({ key: 'LOCK' })
try {
  // Observe the app while the device is locked.
} finally {
  await device.pressKey({ key: 'UNLOCK' })
}
```

Unlock uses WDA's system unlock interaction. It does not enter or bypass a
passcode, dismiss biometric requirements, or change device security settings.
A secured device may require its owner to authenticate. These values are
supported by the native iOS WDA driver, not tvOS, web, or the Appium driver.
Android's existing power-key behavior is unchanged.

Keep the daemon session attached across both commands. Do not relaunch the app
between lock and unlock when testing its background recovery. Restore the
unlocked state during test cleanup, including when an assertion fails.
