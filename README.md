<p align="center"><img src="docs/images/logo.svg" width="96" height="96" alt="LectureFlow"></p>

<h1 align="center">LectureFlow</h1>

<p align="center"><a href="README.md">English</a> · <a href="README.zh-CN.md">简体中文</a> · <a href="README.yue-HK.md">粵語</a></p>

<p align="center">
  <a href="https://github.com/ShouldBeQuincy/LectureFlow/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/ShouldBeQuincy/LectureFlow/actions/workflows/ci.yml/badge.svg"></a>
  <a href="https://github.com/ShouldBeQuincy/LectureFlow/releases"><img alt="Release" src="https://img.shields.io/github/v/release/ShouldBeQuincy/LectureFlow?include_prereleases"></a>
  <a href="LICENSE"><img alt="License: AGPL-3.0-or-later" src="https://img.shields.io/badge/license-AGPL--3.0--or--later-blue"></a>
</p>

**A calm second chance for everyone who can't catch it all the first time.**

A simple-looking, not-so-simple lecture transcription and translation tool. Maybe more.

LectureFlow is a lecture tool that runs on your own computer. Drop in a lecture recording, and it transcribes the speech locally, fixes misheard terms using your course outline and slides, translates it into your language, and then plays the lecture back to you in a player whose subtitles never cover the slides.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/readme/player-en-dark.webp">
  <img alt="The LectureFlow player: the slides on top, bilingual subtitles in their own band below, and the sentence-by-sentence transcript on the right" src="docs/images/readme/player-en-light.webp">
</picture>

<sub>Screenshots use made-up demo data and a synthetic slide.</sub>

## Why LectureFlow exists

**1. "Smart captions", paid for with tuition**
Universities pay serious money every year for commercial lecture-capture platforms. What they get: captions that turn "cache" into "cash", as if the lecturer had moved on to personal finance; five lines of text parked in the middle of the slides, so you get either the formula or the caption, never both; and a player that still can't "go back one sentence". The university pays; the students suffer. LectureFlow sets out to prove that an ordinary laptop can fix all of this.

**2. AI knows everything, except where the lecturer said it**
Paste a whole lecture transcript into an AI, and it will explain it all with great confidence. Which minute of the lecture a sentence came from? No idea, and no way for you to check. Every sentence LectureFlow exports carries a citation accurate to the second, so whatever AI writes your notes, every claim it makes can jump straight back to the original audio.

**3. It assumes you're a native English speaker**
Lecture-capture captions come in one language. But when you study in a second language, what stops you is rarely the knowledge — it's the word you didn't catch, the sentence that went by too fast. LectureFlow translates the whole lecture into your language, sentence by sentence, side by side.

**4. The smoother the translation, the more natural the mistakes**
Machine translation without a glossary can only guess: is "block" a block, or a cipher block? Pure luck. It also swallows the lecturer's slips and the recognition errors whole, and turns them into perfectly grammatical nonsense. LectureFlow first builds a glossary for your course from the outline and slides, then fixes recognition errors using the context of the whole lecture, and only then translates.

**5. A monthly fee for a summary you can't check**
Plenty of "turn your lecture into notes in one click" products charge by the month: they upload your recording to their servers, transcribe it with speech recognition that knows nothing about your course, and write a beautiful summary you have no way to verify. Your recording has left your computer, and your notes are locked inside their app. LectureFlow doesn't write your notes. It makes every note checkable: the recording stays on your computer, the foundation is fixed first, citations are accurate to the second, and the exported files are yours forever.

## Who it is for

Classes assume that everyone can focus for a full hour, understand everything the first time, and take notes on the spot. Many people can't — and that has nothing to do with how smart they are:

- **International students studying in a second language** — you shouldn't have to choose between following the English and following the ideas.
- **Students with limited energy** — those working part-time, caring for family, or returning to study. Watch later, or in several short sittings.
- **Students managing attention or mental health** — lost focus? Jump back to the last sentence. Didn't get it? Loop that sentence as many times as you need.
- **Students who are hard of hearing or read more slowly** — accurate captions for technical terms, adjustable text size and playback speed.
- **Students with nothing but an ordinary laptop** — no expensive graphics card needed, and it can cost nothing at all.

If you have ever felt anxious after missing a class, or kept dragging a progress bar back and forth over a part you couldn't hear, this tool is for you.

## What it does

**Turns recordings into study-ready transcripts**

- Transcribes lectures on your own computer, sentence by sentence, with timestamps.
- Builds a glossary from your course outline and slides, cleaned up by a second "janitor" model, so technical terms are right from the first sentence.
- Fixes misheard words and translates sentence by sentence, using a summary of the whole lecture and the surrounding sentences. Changes it isn't sure about are flagged, and you can play the original audio to check them.
- Writes a bilingual title, a one-sentence summary, and chapters for each lecture.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/readme/lecture-en-dark.webp">
  <img alt="A lecture's page: pipeline progress, token use and cost, and the bilingual transcript with markers for automatic fixes and lines to check" src="docs/images/readme/lecture-en-light.webp">
</picture>

**A player designed for studying**

- Subtitles sit in their own band below the video, so **they never cover the slides or the board**.
- The transcript scrolls with playback; click any sentence to jump to it.
- Previous sentence, next sentence, loop a sentence, slow down — all with keyboard shortcuts.
- Picks up where you left off; copy a citation accurate to the second in one click.

**Organises your courses for you**

- An AI writes a rule that reads recording dates from your file names; lectures are numbered by calendar (e.g. `W02L1`: week 2, session 1) and grouped by teaching week.
- Tells lectures, tutorials, and seminars apart.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/readme/course-en-dark.webp">
  <img alt="A course page: lectures numbered by calendar and grouped by teaching week, with drafts, progress and the live resources panel" src="docs/images/readme/course-en-light.webp">
</picture>

**Transcription that minds your battery**

- Picks an NVIDIA GPU, Intel integrated graphics or NPU, or Apple Silicon to transcribe.
- Laptops use one transcription device at a time: the dedicated GPU when plugged in, the more efficient NPU on battery. Tasks that can't move are paused until you plug back in.
- Optionally borrow a Google Colab GPU: LectureFlow starts it, works through your waiting lectures on it, fetches the results, and shuts it down — not a penny wasted.

**Calm, never pushy**

- Add a lecture, press Start once, and everything else happens on its own. You don't need to watch it.
- Reviewing the corrections is optional: look through them when you have the energy, or just study from the finished transcript when you don't.
- If something goes wrong, it tells you in plain words what happened and what you can do.

**Works with your other tools**

- Export bilingual transcripts, bilingual subtitles (VTT), and a JSON file with timestamped citations that other AI tools can use to make notes.
- The browser extension Send to LectureFlow, for Chrome and Firefox (Edge installs it from the Chrome Web Store; see [docs/install.md](docs/install.md#browser-extension)), sends the recording you are watching on Panopto or another lecture-capture site to LectureFlow with one click and one confirmation. On Panopto you can also send from the page itself, a lecture or a whole page of a course at once. It reads only the media tracks, never your course information.

## Privacy

- **Audio and transcription stay on your computer.** Nothing is uploaded.
- Only if you set up a model provider is the transcribed text sent to **the provider you chose**, for correction, summaries, and translation; when it works out a lecture's title, the lecture's original file name (for lectures sent from the browser extension, the page or platform title) goes along with the summary. You can also choose "transcription only" and use no online service at all.
- With the Google Colab backend, your recordings are uploaded to Google's servers; you'll be told clearly when you choose it. LectureFlow also asks Google, with your own authorization, for your Colab account's compute-unit balance and the GPUs you may borrow, and, only while a runtime is in use, for that runtime's memory and GPU readings to show in the resources panel.
- Saved API keys and your Colab authorization are stored encrypted, protected by your system account (DPAPI on Windows, the Keychain on macOS). If you move the data folder to another computer or user account, you enter them again.
- LectureFlow collects no usage data. No telemetry, no accounts.
- Web mode (running the server program directly and opening it in a browser, without the desktop app) has no access token, so other accounts and programs on this computer can reach it (it listens only on this computer's own address, so other computers on your network cannot). On a shared computer, use the desktop app.
- The browser extension talks only to LectureFlow on your computer, and reads the recording site's cookies only when you send a lecture: see its [privacy policy](docs/extension/privacy.md).

## Transcription backends

| Backend | Status | In a sentence |
| --- | --- | --- |
| NVIDIA CUDA | Supported | You can even use an A100! If you have one. ~~(The maintainer needs six A100s to test this project.)~~ |
| Intel integrated graphics (Iris Xe / Arc / Xe3) | Supported | Surprisingly strong; with the right settings it trades blows with a dedicated GPU. |
| Intel NPU | Supported | For thin-and-light laptops with the "Copilot+ PC" label, on battery. A Copilot key alone isn't enough. |
| Apple MLX | Supported | Courage. Tested on a Mac mini (Apple M6); not yet on a MacBook running on battery. |
| Google Colab | Supported | Borrow Google's GPU: started, used, and shut down for you. Paid compute recommended; the free tier is best-effort. |
| AMD GPUs / APUs | Planned \* ? | We haven't found a device to test on yet. We will. |
| CPU | Desktop fallback only ! | It's all compute in the end — as long as it can keep up. Never used on laptops. |

## Model providers (correction, summaries, translation)

| Provider | Supports | In a sentence |
| --- | --- | --- |
| DeepSeek | Everything, **recommended default** | Cheap: a lecture costs a few cents. Output is occasionally unstable, so retries and quality checks pick up the slack. |
| Claude | Everything | Pricier, but steadier. If you have API credits, use it for correction and glossary cleanup. |
| Gemini | Everything; main pick for the free plan | The free tier is generous enough to live on; the catch is rate limits, so let it work slowly overnight. |
| ChatGPT | Basic support | Sam Ctrl+Alt+man. Works fine; your wallet may not. |
| OpenRouter | Everything | One counter, every model. The maintainer has never actually eaten there. |
| Local Ollama | Translation only; automation on the roadmap | Your GPU is crying, but you already paid for it. We recommend Gemma 4 14B — the results are good. |
| DeepL / Hunyuan | Roadmap, translation only | Sentence by sentence, no context: slips of the tongue and pronouns suffer noticeably. |
| Other OpenAI / Anthropic-compatible APIs | Untested | In theory, anything compatible plugs in. Whether it works is between you and the universe. |

## What it costs

- **Transcription is always free** — it runs on your computer (Colab uses your own compute credits).
- **Correction and translation** use large language models. At typical current prices, a one-hour lecture usually costs a few cents.
- The setup guide offers several plans: **Standard**, **Budget**, **Free** (free tiers, slower), and **Transcription only** (costs nothing). You can change it at any time.

Status notes: \* may also support doing transcription and translation fully on-device, depending on how much the developer sleeps and on friends' test results; ? supported in theory but not fully tested; ! we really don't recommend it — may ~~catch fire~~ ruin your sleep.

## Getting started

1. Download the installer for your system from [Releases](https://github.com/ShouldBeQuincy/LectureFlow/releases): `LectureFlow-Setup-3.3.0-x64.exe` (or the portable build) for Windows, `LectureFlow-3.3.0-arm64.dmg` for Apple Silicon Macs. What's new: [release notes](docs/release-notes/3.3.0.md).
2. The installers are not code-signed yet, so your system will show a security warning. See [docs/install.md](docs/install.md) for how to continue.
3. Open LectureFlow and follow the short setup guide: choose a language, check your computer, choose a plan, download components. You can skip any step and come back later.
4. Add a course: give it a name and a start date, and optionally paste the course outline and upload slides (not required, but they noticeably improve accuracy on technical terms).
5. Drop in a lecture recording and press Start. When it's done, press Play.

## Interface languages

English · 简体中文 · 繁體中文（台灣） · 粵語（香港） · Русский · हिन्दी

<img alt="The same lecture page in the six interface languages" src="docs/images/readme/languages.webp">

Apart from Simplified Chinese, the interface text was translated by AI and reviewed by AI in the role of native speakers. If you are a native speaker of one of these languages, your improvements are very welcome.

## Limitations

- **So far it has only been tested by computer science students**, with lectures in English translated into Simplified Chinese. Medicine, law, the humanities and other subjects use different terminology and teaching styles, so results may differ. Please tell us how it goes.
- On macOS it has been tested on a Mac mini (Apple M6, macOS 27) only; MacBooks running on battery have not been tried yet.
- Whisper's large-v3 models (including large-v3-turbo, the default on Macs) often leave out punctuation and capital letters; large-v2 does not. Since 3.2.0 LectureFlow repairs this in English lectures: with NVIDIA, the CPU or Colab such stretches are transcribed again, and on every backend punctuation and capitals are restored from the rest of the lecture. Lectures transcribed earlier can be repaired with "Fix sentence breaks". The repair is good, not perfect.
- The browser extension knows Panopto best; other sites get generic track detection. It does not handle DRM-protected media, and does not work on Linux, ChromeOS or phones.
- The browser extension and LectureFlow do not handle YouTube or online course platforms (MOOCs and paid courses such as Coursera, edX or Udemy): they are for the lecture recordings your own school gives you.
- AMD ROCm is not supported yet; scheduling across multiple inference backends is not smart yet.
- Automatic transcription, correction, and translation can all make mistakes. For anything important, check the original audio.
- It is designed for **knowledge content with reference material**, not as a general video subtitle tool. For films, anime, or live streams, please use a better-suited tool.

## What it does not replace

- It **does not replace attending class**, or the formal accessibility support your university provides. If you need captioning or note-taking support your university is required to provide, please contact the relevant office.
- **Please follow your university's rules on recording lectures.** LectureFlow is meant for recordings your university provides to you. Recording a class yourself without permission may break your university's rules.

## About this repository: a "rogue open source" notice

This is an open-source project, but the way it is open may not be what you're used to. Let's be upfront:

**How the code was written**

- The code here is almost entirely AI-generated (Claude Code). The maintainer decides what to build, for whom, and how far to take it; writes the requirements and specifications; makes the trade-offs; and does the acceptance testing.
- **The maintainer has not reviewed every line of code by hand.** Quality comes from automated tests (unit tests, end-to-end tests, licence audits), cross-review between several AIs, and **the maintainer using it to study every day**. It is the maintainer's own tool first; when it breaks, they're the first to complain.
- If you have concerns about AI-generated code, that's completely reasonable. Please do whatever review you consider necessary before using, deploying, or modifying it.

**What is and isn't in the repository**

- Included: the full source code, tests, user documentation, licence, and third-party notices.
- Not included: working files from development — progress logs, decision logs, requirement specifications, and AI agent instructions and role definitions. If you want to develop with AI tools, please write your own instruction files from the code and documentation.

**Why the commit history looks so "coarse"**

- Commits from external contributors are kept as they are, with authorship unchanged.
- AI-generated commits are squashed at each release: each run of consecutive AI commits becomes one commit, placed in the original order alongside external commits.
- Commits that only touch documentation (docs) or maintenance chores (chore) are kept as they are.
- When using `git bisect`, AI-generated work can only be narrowed down to these squashed commits. Please include the version number when reporting issues; the maintainer has the full internal history.

We do this because the project is, first, the maintainer's "selfish-first" tool, and only second an open-source project. We'd rather state the rules clearly than pretend every line was written and reviewed by hand. If you prefer the traditional way, the licence lets you fork it and maintain it your own way.

## Roadmap

- More lecture languages: Japanese, Korean, German, French
- A remote-host (SSH) backend; AMD ROCm support
- Transcription checkpoints split at silences
- Ask questions about a whole course in the player, with answers that cite exact moments
- A human proofreading and caption-editing interface for university accessibility services: AI drafts, people approve

## Contributing

Issues, ideas, and code are all welcome. Please read [CONTRIBUTING.md](CONTRIBUTING.md) first; code contributions require signing the Contributor Licence Agreement ([CLA.md](CLA.md)). Especially welcome: native speakers improving interface translations; testing transcription speed and accuracy on different computers; lecture recordings that can be shared openly (e.g. under CC0) together with human-checked transcripts.

This project serves the maintainer's own studies first, so some requests may not be accepted. A "no" doesn't mean your idea is bad — only that time here is limited.

## Licence

LectureFlow is released under the GNU Affero General Public License, version 3 or later (AGPL-3.0-or-later); see [LICENSE](LICENSE). If you modify LectureFlow and offer it to others over a network, you must make your modified source code available to them. An additional permission under section 7 of the licence ([LICENSE-EXCEPTION.md](LICENSE-EXCEPTION.md)) allows combining LectureFlow with the proprietary or otherwise AGPL-incompatible libraries listed there, such as NVIDIA CUDA, which the component manager downloads on your computer; it does not change the other conditions of the AGPL. Licences for third-party components are listed in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

## Thanks

Thanks to Whisper, faster-whisper, OpenVINO, FFmpeg, Electron, and every open-source project that made this possible. The [sample lecture](assets/sample/SOURCE.md) comes from the LibriVox public-domain recording of Michael Faraday's *The Chemical History of a Candle*.

And thanks to everyone who has stayed up late trying to understand a lecture from a recording. This tool is for you.
