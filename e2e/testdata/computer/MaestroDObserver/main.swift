// MaestroDObserver: the e2e window for maestro-d's computer device.
//
//   MaestroDObserver --out events.jsonl   cover the main screen with four
//                                          colour quadrants and write one
//                                          JSON line per input event
//   MaestroDObserver --screen-info        print the main screen's point size
//                                          and backing scale, then exit
//   MaestroDObserver --flags-state        print the modifier flags and mouse
//                                          buttons held system-wide, then exit
//
// It reads only events delivered to its own window, so it needs no
// permission. Build: xcrun swiftc -O -o MaestroDObserver main.swift
import AppKit

let args = CommandLine.arguments

func printJSON(_ value: [String: Any]) {
    let data = try! JSONSerialization.data(withJSONObject: value, options: [.sortedKeys])
    FileHandle.standardOutput.write(data)
    FileHandle.standardOutput.write("\n".data(using: .utf8)!)
}

if args.contains("--screen-info") {
    let s = NSScreen.main!
    printJSON(["width": s.frame.width, "height": s.frame.height, "scale": s.backingScaleFactor])
    exit(0)
}
if args.contains("--flags-state") {
    printJSON([
        "flags": CGEventSource.flagsState(.hidSystemState).rawValue & 0x00ff0000,
        "left": CGEventSource.buttonState(.hidSystemState, button: .left),
        "right": CGEventSource.buttonState(.hidSystemState, button: .right),
    ])
    exit(0)
}

guard let outIndex = args.firstIndex(of: "--out"), outIndex + 1 < args.count else {
    FileHandle.standardError.write("usage: MaestroDObserver --out FILE | --screen-info | --flags-state\n".data(using: .utf8)!)
    exit(2)
}
let outPath = args[outIndex + 1]
FileManager.default.createFile(atPath: outPath, contents: nil)
let out = FileHandle(forWritingAtPath: outPath)!

final class Quadrants: NSView {
    override func draw(_ dirty: NSRect) {
        let w = bounds.width / 2, h = bounds.height / 2
        // Cocoa's origin is bottom-left: red and green are the top half.
        NSColor(srgbRed: 1, green: 0, blue: 0, alpha: 1).setFill(); NSRect(x: 0, y: h, width: w, height: h).fill()
        NSColor(srgbRed: 0, green: 1, blue: 0, alpha: 1).setFill(); NSRect(x: w, y: h, width: w, height: h).fill()
        NSColor(srgbRed: 0, green: 0, blue: 1, alpha: 1).setFill(); NSRect(x: 0, y: 0, width: w, height: h).fill()
        NSColor(srgbRed: 1, green: 1, blue: 1, alpha: 1).setFill(); NSRect(x: w, y: 0, width: w, height: h).fill()
        let label = "MAESTRO-D-E2E" as NSString
        let attrs: [NSAttributedString.Key: Any] = [.font: NSFont.boldSystemFont(ofSize: 48), .foregroundColor: NSColor.black, .backgroundColor: NSColor.yellow]
        let size = label.size(withAttributes: attrs)
        label.draw(at: NSPoint(x: bounds.midX - size.width / 2, y: bounds.midY - size.height / 2), withAttributes: attrs)
    }
    override var acceptsFirstResponder: Bool { true }
    override func keyDown(with event: NSEvent) {}  // no beep
    override func acceptsFirstMouse(for event: NSEvent?) -> Bool { true }
}

final class CoverWindow: NSWindow {
    override var canBecomeKey: Bool { true }
    override var canBecomeMain: Bool { true }
}

let app = NSApplication.shared
app.setActivationPolicy(.regular)
let screen = NSScreen.main!
let window = CoverWindow(contentRect: screen.frame, styleMask: [.borderless], backing: .buffered, defer: false)
window.level = NSWindow.Level(rawValue: Int(CGWindowLevelForKey(.mainMenuWindow)) + 1)
window.contentView = Quadrants(frame: screen.frame)
window.acceptsMouseMovedEvents = true
window.makeKeyAndOrderFront(nil)
window.makeFirstResponder(window.contentView)

let names: [NSEvent.EventType: String] = [
    .keyDown: "keyDown", .keyUp: "keyUp", .flagsChanged: "flagsChanged", .mouseMoved: "mouseMoved",
    .leftMouseDown: "leftMouseDown", .leftMouseUp: "leftMouseUp", .leftMouseDragged: "leftMouseDragged",
    .rightMouseDown: "rightMouseDown", .rightMouseUp: "rightMouseUp",
    .otherMouseDown: "otherMouseDown", .otherMouseUp: "otherMouseUp", .scrollWheel: "scrollWheel",
]
_ = NSEvent.addLocalMonitorForEvents(matching: [.keyDown, .keyUp, .flagsChanged, .mouseMoved, .leftMouseDown, .leftMouseUp, .leftMouseDragged, .rightMouseDown, .rightMouseUp, .otherMouseDown, .otherMouseUp, .scrollWheel]) { event in
    var line: [String: Any] = ["type": names[event.type] ?? "\(event.type.rawValue)", "t": Date().timeIntervalSince1970]
    let m = event.modifierFlags
    var flags: [String] = []
    if m.contains(.command) { flags.append("command") }
    if m.contains(.shift) { flags.append("shift") }
    if m.contains(.option) { flags.append("option") }
    if m.contains(.control) { flags.append("control") }
    line["flags"] = flags
    switch event.type {
    case .keyDown, .keyUp:
        line["characters"] = event.characters ?? ""
        line["charactersIgnoringModifiers"] = event.charactersIgnoringModifiers ?? ""
        line["keyCode"] = event.keyCode
    case .flagsChanged:
        line["keyCode"] = event.keyCode
    case .scrollWheel:
        line["scrollingDeltaX"] = event.scrollingDeltaX
        line["scrollingDeltaY"] = event.scrollingDeltaY
    default:
        line["clickCount"] = event.type == .mouseMoved || event.type == .leftMouseDragged ? 0 : event.clickCount
    }
    let p = NSEvent.mouseLocation
    line["x"] = p.x
    line["y"] = p.y
    let data = try! JSONSerialization.data(withJSONObject: line, options: [.sortedKeys])
    out.write(data)
    out.write("\n".data(using: .utf8)!)
    return event
}

signal(SIGTERM) { _ in exit(0) }
app.activate(ignoringOtherApps: true)
FileHandle.standardOutput.write("ready\n".data(using: .utf8)!)
app.run()
