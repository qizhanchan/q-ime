# Licenses

q-ime contains two kinds of material with different licenses: its own source
code, and word-list data compiled from other projects.

## Source code — MIT

All Go, Objective-C, C and shell source in this repository, and the compiled
input-method binary built from it, are licensed under the MIT License in
[`LICENSE`](LICENSE).

The rime-ice checkout used to build the dictionaries is **not** part of this
repository's source: it is a git submodule at `third_party/rime-ice` with its
own GPL-3.0-only license, fetched separately. Nothing in the MIT-licensed
source is derived from it.

## Dictionary and word-list data — GPL-3.0-only

The pinyin lexicon, the English word list and the emoji table are all derived
from [rime-ice](https://github.com/iDvel/rime-ice), which is licensed
**GPL-3.0-only**. That license carries over to the compiled data.

| Artifact | Derived from | Committed? | License |
|---|---|---|---|
| `third_party/tencent_lite.dict.yaml` | [rime-ice PR #1610](https://github.com/iDvel/rime-ice/pull/1610) (`airx/rime-ice` @ `a3bd0fc8`), a refined subset of rime-ice's `tencent.dict.yaml` | yes | GPL-3.0-only |
| `build/lexicon.bin` | rime-ice `cn_dicts/*.dict.yaml` + `tencent_lite.dict.yaml` | no — built locally by `tools/dictc` | GPL-3.0-only |
| `internal/english/data/english.txt` | rime-ice `en_dicts/*.dict.yaml` | yes | GPL-3.0-only |
| `internal/emoji/data/emoji.txt` | rime-ice `others/emoji-map.txt` | yes | GPL-3.0-only |

rime-ice's own upstreams for the English list are `google-10000-english` and
`rime-melt`; consult those projects for their terms.

`tencent_lite.dict.yaml` comes from an **open, unmerged** pull request against
rime-ice, by a third-party contributor (`airx`). It is committed at the PR
head so the build is offline and reproducible; if that fork ever disappears,
the file is still here under GPL-3.0-only.

### What this means for redistribution

- The **source repository** is MIT-licensed code plus GPL-3.0-only data files,
  with the two clearly separated by this notice.
- **`build/lexicon.bin` is not committed.** `build.sh` compiles it on the
  builder's machine from the rime-ice submodule and the committed lite file.
- **A distributed `.app`, installer or archive that bundles any of the GPL
  data above — including a compiled `lexicon.bin` or the embedded
  `english.txt` / `emoji.txt` — is a combined work and must be distributed
  under GPL-3.0-only**, with the corresponding source made available.

To ship a fully MIT product, replace the data: the engine only depends on the
formats emitted by `tools/dictc` and `tools/engc`, not on rime-ice itself.
