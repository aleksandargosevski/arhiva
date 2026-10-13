// Shows a small floating panel next to the mouse with the given files; dragging it drags all of them.
// Usage: osascript -l JavaScript dragout.js <file>...   Prints "dropped" or "cancelled".
ObjC.import("AppKit");

const MAX_ROWS = 6;
const ROW_H = 28;
const WIDTH = 280;
const TIMEOUT_MS = 2 * 60 * 1000;

function run(paths) {
  if (paths.length === 0) throw new Error("no files given");
  const app = $.NSApplication.sharedApplication;
  app.setActivationPolicy($.NSApplicationActivationPolicyAccessory);
  app.finishLaunching;
  const ws = $.NSWorkspace.sharedWorkspace;

  const state = { dragging: false, ended: false, operation: 0 };

  ObjC.registerSubclass({
    name: "ArhivaDragView",
    superclass: "NSView",
    methods: {
      "acceptsFirstMouse:": { types: ["bool", ["id"]], implementation: () => true },
      // A transparent view would otherwise move the panel instead of getting the drag.
      "mouseDownCanMoveWindow": { types: ["bool", []], implementation: () => false },
      "mouseDown:": { types: ["void", ["id"]], implementation: () => {} },
      "mouseDragged:": {
        types: ["void", ["id"]],
        implementation: function (event) {
          if (state.dragging) return;
          state.dragging = true;
          const items = paths.map((p, i) => {
            const item = $.NSDraggingItem.alloc.initWithPasteboardWriter($.NSURL.fileURLWithPath(p));
            const offset = Math.min(i, 4) * 6;
            item.setDraggingFrameContents($.NSMakeRect(offset, offset, 48, 48), ws.iconForFile(p));
            return item;
          });
          this.beginDraggingSessionWithItemsEventSource($(items), event, this);
        },
      },
      "draggingSession:sourceOperationMaskForDraggingContext:": {
        types: ["unsigned long", ["id", "long"]],
        implementation: () => $.NSDragOperationCopy,
      },
      "draggingSession:endedAtPoint:operation:": {
        types: ["void", ["id", "CGPoint", "unsigned long"]],
        implementation: (session, point, operation) => {
          state.dragging = false;
          state.ended = true;
          state.operation = Number(operation);
        },
      },
      "keyDown:": {
        types: ["void", ["id"]],
        implementation: function (event) {
          if (event.keyCode === 53) this.window.close; // esc
        },
      },
      "acceptsFirstResponder": { types: ["bool", []], implementation: () => true },
    },
  });

  const rows = Math.min(paths.length, MAX_ROWS);
  const more = paths.length - rows;
  const height = 16 + (rows + (more > 0 ? 1 : 0)) * ROW_H;

  const mouse = $.NSEvent.mouseLocation;
  const frame = $.NSMakeRect(mouse.x + 16, mouse.y - height - 16, WIDTH, height);
  const style = $.NSWindowStyleMaskTitled | $.NSWindowStyleMaskClosable | $.NSWindowStyleMaskUtilityWindow;
  const panel = $.NSPanel.alloc.initWithContentRectStyleMaskBackingDefer(frame, style, $.NSBackingStoreBuffered, false);
  panel.title = paths.length === 1 ? "Drag file" : `Drag ${paths.length} files`;
  panel.level = $.NSFloatingWindowLevel;
  panel.floatingPanel = true;
  panel.hidesOnDeactivate = false;
  panel.releasedWhenClosed = false;

  const content = panel.contentView;
  for (let i = 0; i < rows; i++) {
    const y = height - 8 - (i + 1) * ROW_H;
    const icon = $.NSImageView.imageViewWithImage(ws.iconForFile(paths[i]));
    icon.frame = $.NSMakeRect(10, y + 2, 24, 24);
    content.addSubview(icon);
    content.addSubview(label(paths[i].split("/").pop(), 42, y + 5, WIDTH - 52));
  }
  if (more > 0) {
    const y = height - 8 - (rows + 1) * ROW_H;
    const l = label(`+ ${more} more`, 42, y + 5, WIDTH - 52);
    l.textColor = $.NSColor.secondaryLabelColor;
    content.addSubview(l);
  }
  // Transparent on top of the rows, so a drag can start anywhere in the panel.
  const dragView = $.ArhivaDragView.alloc.initWithFrame(content.bounds);
  content.addSubview(dragView);

  app.activateIgnoringOtherApps(true);
  panel.makeKeyAndOrderFront(null);
  panel.makeFirstResponder(dragView);

  // osascript doesn't run an NSApplication event loop, so events are pumped here; without it the panel
  // gets no clicks, drags or key presses.
  const deadline = Date.now() + TIMEOUT_MS;
  while (Date.now() < deadline && panel.isVisible) {
    const event = app.nextEventMatchingMaskUntilDateInModeDequeue(
      $.NSAnyEventMask, $.NSDate.dateWithTimeIntervalSinceNow(0.1), $.NSDefaultRunLoopMode, true);
    if (!event.isNil()) app.sendEvent(event);
    if (state.ended) {
      if (state.operation !== 0) {
        panel.close;
        return "dropped";
      }
      state.ended = false; // dropped nowhere: keep the panel for another try
    }
  }
  panel.close;
  return "cancelled";
}

function label(text, x, y, w) {
  const l = $.NSTextField.labelWithString(text);
  l.frame = $.NSMakeRect(x, y, w, 18);
  l.lineBreakMode = $.NSLineBreakByTruncatingMiddle;
  return l;
}
