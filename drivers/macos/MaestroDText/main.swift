// MaestroDText: reads one image (PNG or JPEG) on stdin and prints the text
// Vision recognises in it as JSON, one object per line of text:
//   [{"text": "Enter Password", "x": 0.45, "y": 0.91, "w": 0.1, "h": 0.02}]
// Coordinates are fractions of the image, origin top-left. maestro-d's
// computer device uses it to recognise a login window or lock screen before
// it types a password. It reads only the image it is given, so it needs no
// permission.
import Foundation
import ImageIO
import Vision

struct Box: Encodable {
    let text: String
    let x: Double
    let y: Double
    let w: Double
    let h: Double
}

func fail(_ message: String) -> Never {
    FileHandle.standardError.write((message + "\n").data(using: .utf8)!)
    exit(1)
}

let data = FileHandle.standardInput.readDataToEndOfFile()
guard let source = CGImageSourceCreateWithData(data as CFData, nil),
      let image = CGImageSourceCreateImageAtIndex(source, 0, nil) else {
    fail("not an image")
}
let request = VNRecognizeTextRequest()
request.recognitionLevel = .accurate
request.usesLanguageCorrection = false
do {
    try VNImageRequestHandler(cgImage: image, options: [:]).perform([request])
} catch {
    fail("text recognition failed: \(error)")
}
var boxes: [Box] = []
for observation in request.results ?? [] {
    guard let best = observation.topCandidates(1).first else { continue }
    let b = observation.boundingBox // origin bottom-left, normalized
    boxes.append(Box(text: best.string, x: b.minX, y: 1 - b.maxY, w: b.width, h: b.height))
}
let out = try JSONEncoder().encode(boxes)
FileHandle.standardOutput.write(out)
