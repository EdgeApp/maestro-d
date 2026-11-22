# The computer device: driving the Mac maestro-d runs on

maestro-d can drive the Mac it runs on as a device: id `mac`, platform
`macos`, kind `computer`. It takes screenshots and sends mouse and keyboard
input over a VNC connection to the Mac's own Screen Sharing at
`127.0.0.1:5900`, so it needs no accessibility permission, works on the
login window and lock screen, and sees exactly what the display shows.

```sh
maestro-d device list --platform macos
maestro-d attach --device mac
maestro-d tapOnPoint --device mac --x 400 --y 300
maestro-d get screenshot --device mac -o now.jpg
```

```js
const mac = await MaestroD.attach({ device: 'mac' })
await mac.clickOn({ point: '50%, 50%', button: 'right' })
await mac.inputText('Hello')
const jpeg = await mac.screenshot()
const region = await mac.screenshot({ image: 'png', region: { x: 0, y: 0, width: 800, height: 600 } })
```

## Setup

1. Turn on **System Settings → General → Sharing → Screen Sharing**. Under
   its ⓘ, the macOS user maestro-d logs in as must be allowed.
2. Write the Mac's credentials to `<home>/computer.json`, where `<home>` is
   `$MAESTRO_D_HOME` (default `~/.maestro-d`), readable only by you:

   ```sh
   umask 077
   cat > ~/.maestro-d/computer.json <<'JSON'
   { "vncPassword": "…", "macosUser": "paul", "macosPassword": "…" }
   JSON
   ```

   `vncPassword` and `macosPassword` are required, and `macosUser` defaults to
   the user running the daemon. maestro-d refuses a file that others can
   read. The credentials stay in this file: no caller sends them, and nothing
   maestro-d logs, stores or answers contains them.

`device list` shows `mac` with its name, model and macOS version, and a
state:

| state | meaning |
| --- | --- |
| `ready` | Screen Sharing answers, offers the account login, and `computer.json` is usable |
| `credentials-missing` | `computer.json` is missing, readable by others, or lacks a field |
| `login-unavailable` | Screen Sharing does not offer the account login (type 30) |
| `screen-sharing-off` | the Screen Sharing service (`com.apple.screensharing`) is not enabled, or nothing answers on `127.0.0.1:5900` |

## How it logs in

maestro-d logs in with the macOS account: Screen Sharing's own login (RFB
security type 30, a Diffie-Hellman exchange and AES), the one Apple's
Screen Sharing app uses. That lands on the logged-in user's desktop.

The account login proves the client to the server, not the server to the
client: whatever answers on the port receives the macOS password. So
maestro-d logs in only while launchd's Screen Sharing service is enabled
(`launchctl print-disabled system` shows `"com.apple.screensharing" =>
enabled`). launchd then holds port 5900, and no other process can listen
there. When the service is off, the Mac lists as `screen-sharing-off`, and an
attach fails with `DEVICE_ERROR` before connecting.

maestro-d does not log in with the VNC password. A connection that does
opens on a login screen of its own, even while the user is logged in and
unlocked at the console. On macOS 26 that screen was frozen and did not take
the password.

When the account connection shows a login window (nobody logged in) or a
lock screen, maestro-d logs in on it with the same account. Before typing
anything it presses Shift, to wake an idle password field, and reads the
screen's text with Apple's Vision framework through `MaestroDText`, a small
helper it builds with `xcrun swiftc` on first use. It types only on a screen
it recognises:

| screen | recognised by | maestro-d |
| --- | --- | --- |
| password screen | the user's full name in the lower half, centred, with "Enter Password" or "Password" below it | clicks the field, types the password and Return |
| user list | the full name with no password prompt | clicks the name, then handles the password screen it opens |

A screen counts as a login screen only when it also shows the large clock
near the top that the lock screen and the login window draw, and no app
menus. A name-and-password login window (the `SHOWFULLNAME` style) is not
logged in, and neither is any app or web page with name and password
fields, even full screen with the menu bar hidden: nothing is typed there.
Anything else is left alone. The full name is the macOS user's as `id -F`
reports it; a user without one cannot be found on a lock screen or login
window, so the attach fails there with `DEVICE_ERROR`, nothing typed.
maestro-d never reads the session's lock flag:
`CGSessionCopyCurrentDictionary` has reported "locked" while the desktop was
showing. If the login screen does not go away within 15 s, the attach fails
with `DEVICE_ERROR` "the login screen did not accept the macOS password".

## Commands

Screen Sharing passes modifiers at once after a login but drops key presses
and clicks for a few seconds (measured on macOS 26: about 3 s after a first
login, up to about 7 s after a login that follows another session), while
reporting nothing. So the first command that types or clicks waits until
8.5 s after the attach's login. Screenshots never wait.

Coordinates are the screen's physical pixels: 2× the macOS point size on a
Retina display. `"x%, y%"` works too.

| command | on the computer |
| --- | --- |
| `tapOn {point}`, `tapOnPoint` | left click; `longPress` holds 1 s (or `duration`); `repeat: 2` double-clicks |
| `clickOn {point, button, count}` | `left`, `right` or `middle`, 1 to 3 clicks; no point clicks where the pointer is |
| `moveMouse "x, y"` | moves the pointer, keeping a held button down |
| `swipe` | a left-button drag from `start` to `end` (or across 60% of the screen for a direction) |
| `scroll` | five wheel ticks at the centre, `DOWN` by default |
| `scrollAt {point, x, y}` | wheel ticks at a point; positive `y` scrolls down, positive `x` right. macOS coalesces them, and with natural scrolling a positive `y` reads as a negative `scrollingDeltaY` |
| `inputText` | Latin-1 characters as themselves, anything else up to U+FFFF (`€`) as its Unicode keysym. A line break, `\n`, `\r` or `\r\n`, is one Return. Characters beyond U+FFFF (emoji like 🙂) never reach the Mac, so text holding one fails before anything is typed |
| `eraseText` | Backspace × `characters` (default 50) |
| `pressKey` | a key (`Enter`, `Escape`, `Tab`, arrows, `F1`–`F12`, …) or a combination: `Meta+Shift+K`, `Control+E`. `Meta`, `Cmd` and `Command` are Command |
| `takeScreenshot`, `assertScreenshot`, `waitForAnimationToEnd`, `hideKeyboard` | as on phones |

What does not work, and why:

- **Selectors.** The computer has no UI hierarchy, so `tapOn: Login`,
  `assertVisible` and the like fail with "the computer device has no UI
  hierarchy; use a point". `doubleTapOn` and `longPressOn` take only a
  selector; use `tapOn` with `repeat: 2` or `longPress: true`, or `clickOn`.
- **Option.** Screen Sharing gives a VNC client no Option key: Alt, Super and
  Hyper all arrive as Command, and nothing arrives as Option. `pressKey`
  refuses `Alt`/`Option` rather than send Command. Type characters that need
  Option with `inputText` (`é`).
- **Video.** Screen Sharing sends third-party clients pixels (Raw, ZRLE,
  Zlib), never video, so the computer has no stream or recording
  (`CAPTURE_UNAVAILABLE`).
- **Apps, traffic shaping.** `launchApp` and the other app steps are not
  supported; `setTrafficShape` answers `UNSUPPORTED` unless `network.json`
  configures `mac`.

## Screenshots

`GET /v1/devices/mac/screenshot` (and `get screenshot`, `screenshot()`)
answers JPEG by default; `image=png` asks for PNG. `x`, `y`, `width` and
`height` capture just that rectangle, which Screen Sharing sends on its own:
on a 3456 × 2234 display a full JPEG takes about 130 ms after the first
capture, and an 800 × 600 region about 10 ms. Flows (`takeScreenshot`,
`assertScreenshot`) stay PNG.

## Sessions

- **Idle detach.** An attached computer that receives no call for 5 minutes
  is detached and its connection closed (event `device.detached`, message
  `idle`). Attach with `extra: {idleTimeoutSec: N}` to change it, 0 to turn it
  off. The one-shot CLI re-attaches on its own at the next call.
- **Connection lost.** When Screen Sharing ends the connection (it
  restarts or is turned off), `mac` is detached at once (event
  `device.detached`, message `connection closed`), after any step in flight
  answers. The next call answers `DEVICE_NOT_ATTACHED`, and the one-shot CLI
  attaches again.
- **One holder.** Like any device, `mac` belongs to one daemon at a time
  (`DEVICE_IN_USE`).
- **Local only.** A hub never shares the computer, whatever `--share-device`
  says: peers and remote callers cannot see, lease or attach it. Agents on
  the Mac take turns through a hub's local socket: `maestro-d lease --device
  mac`, or a selector with `kind: computer` or `platform: macos`. A selector
  that names none of its kind, platform or name never picks it.
- **What it shows is the console.** The device drives the Mac's physical
  display: what someone at the Mac sees, it sees, and the input it sends
  reaches whatever app is in front.
