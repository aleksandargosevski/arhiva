// Removes the background of an image with the Vision framework (macOS 14+) and writes a PNG.
// Usage: osascript -l JavaScript removebg.js <input> <output.png>
ObjC.import("Foundation");
ObjC.import("Vision");
ObjC.import("CoreImage");
ObjC.import("AppKit");

function run(argv) {
  if (argv.length !== 2) {
    throw new Error("usage: removebg.js <input> <output.png>");
  }
  const [input, output] = argv;
  if (typeof $.VNGenerateForegroundInstanceMaskRequest === "undefined") {
    throw new Error("removing backgrounds needs macOS 14 or newer");
  }

  const handler = $.VNImageRequestHandler.alloc.initWithURLOptions($.NSURL.fileURLWithPath(input), $({}));
  const request = $.VNGenerateForegroundInstanceMaskRequest.alloc.init;
  const err = Ref();
  if (!handler.performRequestsError($([request]), err)) {
    throw new Error(describe(err, "Vision request failed"));
  }

  const observation = request.results.firstObject;
  if (!observation || observation.isNil()) {
    throw new Error("no subject found");
  }
  const buffer = observation.generateMaskedImageOfInstancesFromRequestHandlerCroppedToInstancesExtentError(
    observation.allInstances, handler, false, err);
  if (!buffer) {
    throw new Error(describe(err, "could not mask the image"));
  }

  const image = $.CIImage.imageWithCVPixelBuffer(buffer);
  const png = $.NSBitmapImageRep.alloc.initWithCIImage(image)
    .representationUsingTypeProperties($.NSBitmapImageFileTypePNG, $({}));
  if (!png.writeToFileAtomically(output, true)) {
    throw new Error("could not write " + output);
  }
}

function describe(err, fallback) {
  const e = err[0];
  return e && !e.isNil() ? ObjC.unwrap(e.localizedDescription) : fallback;
}
