// make-icon.m — regenerates ../Resources/QuiIME.pdf.
//
// An input method that has no icon file does not appear in System Settings >
// Keyboard > Input Sources at all: TISRegisterInputSource still returns
// noErr, the bundle still loads, and the input source is simply absent from
// the database. That failure mode cost the first install attempt, so the
// asset is generated from source rather than committed as a mystery blob.
//
// Vector PDF (not PNG) because the menu-bar item is drawn at several sizes,
// and pure black on transparent so AppKit can treat it as a template image
// and invert it for a dark menu bar.
//
//   clang -fobjc-arc -framework Cocoa -o /tmp/make-icon make-icon.m
//   /tmp/make-icon ../Resources/QuiIME.pdf

#import <Cocoa/Cocoa.h>

int main(int argc, const char *argv[]) {
    @autoreleasepool {
        if (argc < 2) {
            fprintf(stderr, "usage: make-icon <out.pdf>\n");
            return 2;
        }
        NSURL *out = [NSURL fileURLWithPath:@(argv[1])];

        const CGFloat side = 16.0;
        CGRect box = CGRectMake(0, 0, side, side);
        CGContextRef ctx =
            CGPDFContextCreateWithURL((__bridge CFURLRef)out, &box, NULL);
        if (ctx == NULL) {
            fprintf(stderr, "make-icon: cannot create PDF at %s\n", argv[1]);
            return 1;
        }
        CGPDFContextBeginPage(ctx, NULL);

        NSGraphicsContext *nsctx =
            [NSGraphicsContext graphicsContextWithCGContext:ctx flipped:NO];
        [NSGraphicsContext saveGraphicsState];
        [NSGraphicsContext setCurrentContext:nsctx];

        // 拼 at a size that fills the 16pt menu-bar box. PingFang is the
        // system Simplified Chinese face; fall back to whatever the system
        // offers for the character if it is ever missing.
        NSFont *font = [NSFont fontWithName:@"PingFangSC-Semibold" size:14.5];
        if (font == nil) {
            font = [NSFont systemFontOfSize:14.5 weight:NSFontWeightSemibold];
        }
        NSDictionary *attrs = @{
            NSFontAttributeName : font,
            NSForegroundColorAttributeName : [NSColor blackColor],
        };
        NSString *glyph = @"拼";
        NSSize sz = [glyph sizeWithAttributes:attrs];
        NSPoint at = NSMakePoint((side - sz.width) / 2.0,
                                 (side - sz.height) / 2.0);
        [glyph drawAtPoint:at withAttributes:attrs];

        [NSGraphicsContext restoreGraphicsState];
        CGPDFContextEndPage(ctx);
        CGPDFContextClose(ctx);
        CGContextRelease(ctx);

        fprintf(stdout, "wrote %s\n", argv[1]);
    }
    return 0;
}
