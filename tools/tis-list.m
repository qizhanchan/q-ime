// tis-list.m — does the OS actually know about our input method?
//
// This exists because the failure mode that blocked step 1 is SILENT:
// TISRegisterInputSource returns noErr, LaunchServices logs a successful
// bundle registration, the process launches and stays up — and the input
// source is still absent from the Text Input Source database, so it never
// appears in System Settings. There is no error to read anywhere except a
// single amfid line in the unified log.
//
// So: query the database directly instead of squinting at System Settings.
//
//   clang -fobjc-arc -Wno-deprecated-declarations \
//       -framework Carbon -framework Foundation -o /tmp/tis-list tis-list.m
//   /tmp/tis-list                 # ours only, with details
//   /tmp/tis-list --all           # every input method, one line each
//   /tmp/tis-list com.sogou       # details for any prefix, to compare against
//                                 # a known-good third-party input method
//
// --all is the control experiment: if third-party entries like
// com.sogou.inputmethod.* show up and ours does not, the query is fine and
// the registration genuinely failed.

#import <Carbon/Carbon.h>
#import <Foundation/Foundation.h>

static const char *kOurPrefix = "com.qizhanchan.inputmethod";

static BOOL isInputMethod(TISInputSourceRef src) {
    NSString *type = (__bridge NSString *)TISGetInputSourceProperty(
        src, kTISPropertyInputSourceType);
    return
        [type isEqualToString:(__bridge NSString *)
                                  kTISTypeKeyboardInputMethodModeEnabled] ||
        [type isEqualToString:(__bridge NSString *)
                                  kTISTypeKeyboardInputMethodWithoutModes] ||
        [type isEqualToString:(__bridge NSString *)kTISTypeKeyboardInputMode];
}

static const char *boolStr(CFBooleanRef b) {
    return (b && CFBooleanGetValue(b)) ? "yes" : "no";
}

int main(int argc, const char *argv[]) {
    @autoreleasepool {
        BOOL all = (argc > 1 && strcmp(argv[1], "--all") == 0);
        // Any non-flag argument overrides the prefix, so the same tool can
        // dump a known-good input method for side-by-side comparison.
        const char *prefix = kOurPrefix;
        if (argc > 1 && strncmp(argv[1], "--", 2) != 0) {
            prefix = argv[1];
        }

        // true = include installed-but-not-enabled sources, which is what a
        // freshly registered input method is before the user adds it.
        CFArrayRef list = TISCreateInputSourceList(NULL, true);
        if (list == NULL) {
            printf("TISCreateInputSourceList returned NULL\n");
            return 1;
        }

        int hits = 0;
        for (CFIndex i = 0; i < CFArrayGetCount(list); i++) {
            TISInputSourceRef src =
                (TISInputSourceRef)CFArrayGetValueAtIndex(list, i);
            NSString *sid = (__bridge NSString *)TISGetInputSourceProperty(
                src, kTISPropertyInputSourceID);
            if (sid == nil) {
                continue;
            }
            BOOL ours = [sid hasPrefix:@(prefix)];
            if (!ours && !(all && isInputMethod(src))) {
                continue;
            }
            NSString *name = (__bridge NSString *)TISGetInputSourceProperty(
                src, kTISPropertyLocalizedName);
            printf("%s %-52s enabled=%-3s selected=%s\n", ours ? "OURS" : "    ",
                   sid.UTF8String,
                   boolStr(TISGetInputSourceProperty(
                       src, kTISPropertyInputSourceIsEnabled)),
                   boolStr(TISGetInputSourceProperty(
                       src, kTISPropertyInputSourceIsSelected)));
            if (ours) {
                hits++;
                if (name) {
                    printf("     name: %s\n", name.UTF8String);
                }
                // Which section of the System Settings picker an input source
                // lands in is decided by these, not by the app's own idea of
                // what language it is for.
                NSArray *langs = (__bridge NSArray *)TISGetInputSourceProperty(
                    src, kTISPropertyInputSourceLanguages);
                printf("     languages: %s\n",
                       langs ? [langs componentsJoinedByString:@","].UTF8String
                             : "(none)");
                printf("     category: %s\n",
                       ((__bridge NSString *)TISGetInputSourceProperty(
                            src, kTISPropertyInputSourceCategory))
                           .UTF8String);
                printf("     type: %s\n",
                       ((__bridge NSString *)TISGetInputSourceProperty(
                            src, kTISPropertyInputSourceType))
                           .UTF8String);
                printf("     selectCapable: %s  enableCapable: %s\n",
                       boolStr(TISGetInputSourceProperty(
                           src, kTISPropertyInputSourceIsSelectCapable)),
                       boolStr(TISGetInputSourceProperty(
                           src, kTISPropertyInputSourceIsEnableCapable)));
            }
        }
        CFRelease(list);

        if (hits == 0) {
            printf("\nNOT in the TIS database — it will not appear in System "
                   "Settings.\nSee the \"blocked on\" section of README.md.\n");
            return 1;
        }
        return 0;
    }
}
