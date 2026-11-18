// MaestroDCapture: maestro-d's screen-capture helper for physical iPhones
// and iPads connected over USB (pkg/capture/quicktime builds, signs and
// launches it; see docs/daemon/streaming.md).
//
// It turns on CoreMediaIO's kCMIOHardwarePropertyAllowScreenCaptureDevices
// (the QuickTime screen-capture channel), opens each device maestro-d asks
// for as an AVCaptureSession, and sends the device's own H.264 samples and
// LPCM audio, undecoded, over a unix socket.
//
// Usage: MaestroDCapture --socket <path> --protocol 1
//
// Protocol 1. The first connection is the control connection: the helper
// sends hello{protocol, pid, version}, then Camera statuses; maestro-d
// writes JSON lines {"op":"open","udid","name"} and {"op":"close","udid"}.
// Each open gets a connection of its own: hello{udid}, statuses
// (device-found, capturing{match}, unavailable{reason}), then media.
// Every frame is a 16-byte little-endian header (type u8, flags u8,
// reserved u16, pts_us i64, len u32) and its payload:
//   1 status       JSON
//   2 video config u32 width, u32 height, avcC atom
//   3 video sample AVCC sample as delivered; flags bit 0 = sync
//   4 audio config u32 sampleRate, u16 channels, u16 bits, u32 asbd flags
//   5 audio sample interleaved PCM
// The helper exits when the control connection closes, or on SIGTERM.

import AVFoundation
import CoreMedia
import CoreMediaIO
import Foundation

let helperVersion = "1"
let protocolVersion = 1

// MARK: - Socket connection

final class Conn {
  let fd: Int32
  private let lock = NSLock()
  private var isClosed = false

  init?(path: String) {
    let fd = socket(AF_UNIX, SOCK_STREAM, 0)
    if fd < 0 { return nil }
    var addr = sockaddr_un()
    addr.sun_family = sa_family_t(AF_UNIX)
    let bytes = Array(path.utf8)
    let capacity = MemoryLayout.size(ofValue: addr.sun_path)
    if bytes.count >= capacity {
      Darwin.close(fd)
      return nil
    }
    withUnsafeMutableBytes(of: &addr.sun_path) { buf in
      for (i, b) in bytes.enumerated() { buf[i] = b }
      buf[bytes.count] = 0
    }
    addr.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
    let rc = withUnsafePointer(to: &addr) { p in
      p.withMemoryRebound(to: sockaddr.self, capacity: 1) {
        connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
      }
    }
    if rc != 0 {
      Darwin.close(fd)
      return nil
    }
    var one: Int32 = 1
    setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &one, socklen_t(MemoryLayout<Int32>.size))
    self.fd = fd
  }

  /// Writes one frame; false once the connection is gone.
  @discardableResult
  func send(type: UInt8, flags: UInt8 = 0, pts: Int64 = 0, payload: Data) -> Bool {
    var header = Data(count: 16)
    header[0] = type
    header[1] = flags
    withUnsafeBytes(of: pts.littleEndian) { header.replaceSubrange(4..<12, with: $0) }
    withUnsafeBytes(of: UInt32(payload.count).littleEndian) { header.replaceSubrange(12..<16, with: $0) }
    lock.lock()
    defer { lock.unlock() }
    if isClosed { return false }
    return writeAll(header) && writeAll(payload)
  }

  @discardableResult
  func status(_ fields: [String: Any]) -> Bool {
    guard let json = try? JSONSerialization.data(withJSONObject: fields) else { return false }
    return send(type: 1, payload: json)
  }

  private func writeAll(_ d: Data) -> Bool {
    if d.isEmpty { return true }
    return d.withUnsafeBytes { (buf: UnsafeRawBufferPointer) -> Bool in
      var off = 0
      while off < buf.count {
        let n = write(fd, buf.baseAddress!.advanced(by: off), buf.count - off)
        if n < 0 {
          if errno == EINTR { continue }
          return false
        }
        off += n
      }
      return true
    }
  }

  func close() {
    lock.lock()
    defer { lock.unlock() }
    if isClosed { return }
    isClosed = true
    shutdown(fd, SHUT_RDWR)
    Darwin.close(fd)
  }
}

// MARK: - Arguments

var socketPath = ""
var requestedProtocol = 0
do {
  var it = CommandLine.arguments.dropFirst().makeIterator()
  while let arg = it.next() {
    switch arg {
    case "--socket": socketPath = it.next() ?? ""
    case "--protocol": requestedProtocol = Int(it.next() ?? "") ?? 0
    default: break
    }
  }
}
if socketPath.isEmpty || requestedProtocol != protocolVersion {
  FileHandle.standardError.write("usage: MaestroDCapture --socket <path> --protocol \(protocolVersion)\n".data(using: .utf8)!)
  exit(2)
}

signal(SIGPIPE, SIG_IGN)
signal(SIGTERM, SIG_IGN)
let termSource = DispatchSource.makeSignalSource(signal: SIGTERM, queue: .main)
termSource.setEventHandler { exit(0) }
termSource.resume()

guard let control = Conn(path: socketPath) else {
  FileHandle.standardError.write("MaestroDCapture: cannot connect to \(socketPath)\n".data(using: .utf8)!)
  exit(3)
}
control.status(["state": "hello", "protocol": protocolVersion, "pid": Int(getpid()), "version": helperVersion])

// MARK: - Device capture

func normalizedID(_ s: String) -> String {
  s.replacingOccurrences(of: "-", with: "").lowercased()
}

func iOSCaptureDevices() -> [AVCaptureDevice] {
  let types: [AVCaptureDevice.DeviceType]
  if #available(macOS 14.0, *) {
    types = [.external]
  } else {
    types = [.externalUnknown]
  }
  return AVCaptureDevice.DiscoverySession(deviceTypes: types, mediaType: .muxed, position: .unspecified)
    .devices.filter { $0.modelID == "iOS Device" }
}

/// The avcC atom of an H.264 format description: from its sample
/// description extension atoms, else built from its parameter sets.
func avcCAtom(_ fd: CMFormatDescription) -> Data? {
  if let atoms = CMFormatDescriptionGetExtension(fd, extensionKey: kCMFormatDescriptionExtension_SampleDescriptionExtensionAtoms) as? [String: Any],
     let avcC = atoms["avcC"] as? Data {
    return avcC
  }
  var count = 0
  var nalLength: Int32 = 0
  guard CMVideoFormatDescriptionGetH264ParameterSetAtIndex(fd, parameterSetIndex: 0, parameterSetPointerOut: nil,
          parameterSetSizeOut: nil, parameterSetCountOut: &count, nalUnitHeaderLengthOut: &nalLength) == noErr else { return nil }
  var sps: [Data] = [], pps: [Data] = []
  for i in 0..<count {
    var p: UnsafePointer<UInt8>?
    var n = 0
    guard CMVideoFormatDescriptionGetH264ParameterSetAtIndex(fd, parameterSetIndex: i, parameterSetPointerOut: &p,
            parameterSetSizeOut: &n, parameterSetCountOut: nil, nalUnitHeaderLengthOut: nil) == noErr, let p, n > 0 else { return nil }
    let nal = Data(bytes: p, count: n)
    switch nal[0] & 0x1f {
    case 7: sps.append(nal)
    case 8: pps.append(nal)
    default: break
    }
  }
  guard let first = sps.first, first.count >= 4, !pps.isEmpty else { return nil }
  var out = Data([1, first[1], first[2], first[3], 0xfc | UInt8(max(nalLength, 1) - 1), 0xe0 | UInt8(sps.count)])
  for s in sps {
    out.append(contentsOf: [UInt8(s.count >> 8), UInt8(s.count & 0xff)])
    out.append(s)
  }
  out.append(UInt8(pps.count))
  for s in pps {
    out.append(contentsOf: [UInt8(s.count >> 8), UInt8(s.count & 0xff)])
    out.append(s)
  }
  return out
}

func blockBytes(_ sb: CMSampleBuffer) -> Data? {
  guard let bb = CMSampleBufferGetDataBuffer(sb) else { return nil }
  let len = CMBlockBufferGetDataLength(bb)
  if len == 0 { return Data() }
  var d = Data(count: len)
  let st = d.withUnsafeMutableBytes { CMBlockBufferCopyDataBytes(bb, atOffset: 0, dataLength: len, destination: $0.baseAddress!) }
  return st == kCMBlockBufferNoErr ? d : nil
}

func microseconds(_ t: CMTime) -> Int64 {
  if !t.isValid { return 0 }
  return CMTimeConvertScale(t, timescale: 1_000_000, method: .default).value
}

func appendLE<T: FixedWidthInteger>(_ d: inout Data, _ v: T) {
  withUnsafeBytes(of: v.littleEndian) { d.append(contentsOf: $0) }
}

/// Devices being captured, by UDID; touched on the main queue only.
var devices: [String: DeviceCapture] = [:]

final class DeviceCapture: NSObject, AVCaptureVideoDataOutputSampleBufferDelegate, AVCaptureAudioDataOutputSampleBufferDelegate {
  let udid: String
  let name: String
  let conn: Conn
  let queue = DispatchQueue(label: "app.edge.maestro-d.capture.device")
  private let stateLock = NSLock()
  private var stopped = false
  private var session: AVCaptureSession?
  private var observers: [NSObjectProtocol] = []
  private var match = ""
  private var videoFormat: CMFormatDescription?
  private var audioFormat: CMFormatDescription?
  private var sentCapturing = false

  init?(udid: String, name: String) {
    guard let conn = Conn(path: socketPath) else { return nil }
    self.udid = udid
    self.name = name
    self.conn = conn
    super.init()
  }

  var isStopped: Bool {
    stateLock.lock()
    defer { stateLock.unlock() }
    return stopped
  }

  func start() {
    conn.status(["state": "hello", "udid": udid])
    // maestro-d closing this connection stops the capture.
    let fd = conn.fd
    Thread { [weak self] in
      var b = [UInt8](repeating: 0, count: 256)
      while read(fd, &b, b.count) > 0 {}
      self?.stop()
    }.start()
    queue.async { self.discover() }
  }

  /// Finds the device's capture device: by USB serial (its unique ID is
  /// the UDID), else by a unique name; every 250 ms for up to 10 s.
  private func discover() {
    let deadline = Date().addingTimeInterval(10)
    let want = normalizedID(udid)
    var named = 0
    while !isStopped {
      let found = iOSCaptureDevices()
      if let d = found.first(where: { normalizedID($0.uniqueID) == want }) {
        begin(d, match: "serial")
        return
      }
      let byName = found.filter { $0.localizedName == name }
      named = byName.count
      if named == 1 {
        begin(byName[0], match: "name")
        return
      }
      if Date() >= deadline { break }
      Thread.sleep(forTimeInterval: 0.25)
    }
    if isStopped { return }
    if named >= 2 {
      fail("ambiguous-device-name", "two iOS devices are named \(name); rename one")
    } else {
      fail("no-capture-device", "no capture device for \(udid) (\(name)) appeared within 10 s; is it connected over USB and unlocked?")
    }
  }

  private func begin(_ device: AVCaptureDevice, match: String) {
    self.match = match
    conn.status(["state": "device-found", "udid": udid, "match": match,
                 "message": "\(device.localizedName), capture device \(device.uniqueID), matched by \(match)"])
    let s = AVCaptureSession()
    guard let input = try? AVCaptureDeviceInput(device: device), s.canAddInput(input) else {
      fail("helper-failed", "cannot open the capture device of \(udid)")
      return
    }
    s.addInput(input)
    let video = AVCaptureVideoDataOutput()
    video.videoSettings = [:]  // the device's own samples, never decoded
    video.alwaysDiscardsLateVideoFrames = false
    video.setSampleBufferDelegate(self, queue: queue)
    guard s.canAddOutput(video) else {
      fail("helper-failed", "cannot add a video output for \(udid)")
      return
    }
    s.addOutput(video)
    let audio = AVCaptureAudioDataOutput()
    audio.setSampleBufferDelegate(self, queue: queue)
    if s.canAddOutput(audio) { s.addOutput(audio) }
    let center = NotificationCenter.default
    observers.append(center.addObserver(forName: AVCaptureDevice.wasDisconnectedNotification, object: device, queue: nil) { [weak self] _ in
      self?.stop()
    })
    observers.append(center.addObserver(forName: AVCaptureSession.runtimeErrorNotification, object: s, queue: nil) { [weak self] _ in
      self?.stop()
    })
    session = s
    s.startRunning()
  }

  private func fail(_ reason: String, _ message: String) {
    conn.status(["state": "unavailable", "udid": udid, "reason": reason, "message": message])
    stop()
  }

  func captureOutput(_ output: AVCaptureOutput, didOutput sb: CMSampleBuffer, from connection: AVCaptureConnection) {
    if isStopped { return }
    guard let fd = CMSampleBufferGetFormatDescription(sb) else { return }
    let pts = microseconds(CMSampleBufferGetPresentationTimeStamp(sb))
    let ok = output is AVCaptureAudioDataOutput ? sendAudio(sb, fd, pts) : sendVideo(sb, fd, pts)
    if !ok { stop() }
  }

  private func sendVideo(_ sb: CMSampleBuffer, _ fd: CMFormatDescription, _ pts: Int64) -> Bool {
    if CMSampleBufferGetImageBuffer(sb) != nil { return true }  // a decoded frame: not expected, skipped
    if videoFormat == nil || !CMFormatDescriptionEqual(videoFormat!, otherFormatDescription: fd) {
      guard let avcC = avcCAtom(fd) else {
        fail("helper-failed", "the device's video is not H.264")
        return true
      }
      let dims = CMVideoFormatDescriptionGetDimensions(fd)
      var p = Data()
      appendLE(&p, UInt32(dims.width))
      appendLE(&p, UInt32(dims.height))
      p.append(avcC)
      if !conn.send(type: 2, pts: pts, payload: p) { return false }
      videoFormat = fd
    }
    if !sentCapturing {
      sentCapturing = true
      if !conn.status(["state": "capturing", "udid": udid, "match": match]) { return false }
    }
    guard let data = blockBytes(sb) else { return true }
    let atts = CMSampleBufferGetSampleAttachmentsArray(sb, createIfNecessary: false) as? [[CFString: Any]]
    let notSync = (atts?.first?[kCMSampleAttachmentKey_NotSync] as? Bool) ?? false
    return conn.send(type: 3, flags: notSync ? 0 : 1, pts: pts, payload: data)
  }

  private func sendAudio(_ sb: CMSampleBuffer, _ fd: CMFormatDescription, _ pts: Int64) -> Bool {
    guard let asbd = CMAudioFormatDescriptionGetStreamBasicDescription(fd)?.pointee,
          asbd.mFormatID == kAudioFormatLinearPCM else { return true }
    if audioFormat == nil || !CMFormatDescriptionEqual(audioFormat!, otherFormatDescription: fd) {
      var p = Data()
      appendLE(&p, UInt32(asbd.mSampleRate))
      appendLE(&p, UInt16(asbd.mChannelsPerFrame))
      appendLE(&p, UInt16(asbd.mBitsPerChannel))
      appendLE(&p, UInt32(asbd.mFormatFlags))
      if !conn.send(type: 4, pts: pts, payload: p) { return false }
      audioFormat = fd
    }
    guard let data = blockBytes(sb), !data.isEmpty else { return true }
    return conn.send(type: 5, pts: pts, payload: data)
  }

  /// Stops the capture and closes the device's connection; maestro-d sees
  /// the connection end.
  func stop() {
    stateLock.lock()
    if stopped {
      stateLock.unlock()
      return
    }
    stopped = true
    stateLock.unlock()
    for o in observers { NotificationCenter.default.removeObserver(o) }
    queue.async {
      self.session?.stopRunning()
      self.session = nil
      self.conn.close()
    }
    let udid = self.udid
    DispatchQueue.main.async {
      if devices[udid] === self { devices[udid] = nil }
    }
  }
}

// MARK: - Control

var ready = false
var pending: [(udid: String, name: String)] = []

func openDevice(udid: String, name: String) {
  if let old = devices[udid] { old.stop() }
  guard let d = DeviceCapture(udid: udid, name: name) else { return }
  devices[udid] = d
  d.start()
}

func handle(op: String, udid: String, name: String) {
  switch op {
  case "open":
    if ready {
      openDevice(udid: udid, name: name)
    } else {
      pending.append((udid, name))
    }
  case "close":
    pending.removeAll { $0.udid == udid }
    devices[udid]?.stop()
  default:
    break
  }
}

/// Camera access is granted: switch the screen-capture devices on, once,
/// and open what maestro-d asked for meanwhile.
func becomeReady() {
  var prop = CMIOObjectPropertyAddress(
    mSelector: CMIOObjectPropertySelector(kCMIOHardwarePropertyAllowScreenCaptureDevices),
    mScope: CMIOObjectPropertyScope(kCMIOObjectPropertyScopeGlobal),
    mElement: CMIOObjectPropertyElement(kCMIOObjectPropertyElementMain))
  var allow: UInt32 = 1
  let st = CMIOObjectSetPropertyData(CMIOObjectID(kCMIOObjectSystemObject), &prop, 0, nil,
                                     UInt32(MemoryLayout<UInt32>.size), &allow)
  if st != 0 {
    control.status(["state": "unavailable", "reason": "helper-failed",
                    "message": "kCMIOHardwarePropertyAllowScreenCaptureDevices: status \(st)"])
    exit(1)
  }
  ready = true
  control.status(["state": "ready"])
  for p in pending { openDevice(udid: p.udid, name: p.name) }
  pending.removeAll()
}

func denied() {
  control.status(["state": "unavailable", "reason": "camera-permission",
                  "message": "macOS denied maestro-d capture access to the camera; allow it in System Settings > Privacy & Security > Camera"])
  exit(0)
}

Thread {
  var buf = Data()
  var chunk = [UInt8](repeating: 0, count: 4096)
  while true {
    let n = read(control.fd, &chunk, chunk.count)
    if n <= 0 { exit(0) }  // maestro-d is gone
    buf.append(contentsOf: chunk[0..<n])
    while let nl = buf.firstIndex(of: 0x0a) {
      let line = buf.subdata(in: buf.startIndex..<nl)
      buf.removeSubrange(buf.startIndex...nl)
      guard let obj = (try? JSONSerialization.jsonObject(with: line)) as? [String: Any],
            let op = obj["op"] as? String, let udid = obj["udid"] as? String else { continue }
      let name = obj["name"] as? String ?? ""
      DispatchQueue.main.async { handle(op: op, udid: udid, name: name) }
    }
  }
}.start()

switch AVCaptureDevice.authorizationStatus(for: .video) {
case .authorized:
  becomeReady()
case .notDetermined:
  control.status(["state": "waiting-permission"])
  AVCaptureDevice.requestAccess(for: .video) { granted in
    DispatchQueue.main.async { granted ? becomeReady() : denied() }
  }
default:
  denied()
}

RunLoop.main.run()
