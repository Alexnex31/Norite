# Where these recordings come from

Three excerpts from Wikimedia Commons, each dedicated to the public domain by its author. They were
chosen for the DSP's tests (M30), downloaded on 2026-11-09, cut to 12 s, made mono at 48 kHz,
peak-normalised to about -3 dBFS, and encoded by `../gen.go` to Opus packets at 96 kbps.

| File | Source | Author | Terms | SHA-256 of the download |
| --- | --- | --- | --- | --- |
| `near.opus` | [File:'Absent-minded professor' from Wikipedia.ogg](https://commons.wikimedia.org/wiki/File:%27Absent-minded_professor%27_from_Wikipedia.ogg), seconds 20–32 | AlexRexed, "Own work" | [CC0 1.0](https://creativecommons.org/publicdomain/zero/1.0/) | `a5486d42dfc18144228043eebb10a6c4da560fd5425ad528037b90838067fda3` |
| `far.opus` | [File:Sonnet-by Christina Rossetti.ogg](https://commons.wikimedia.org/wiki/File:Sonnet-by_Christina_Rossetti.ogg), seconds 4–16 | User:Shanel, "Own work"; the poem is Christina Rossetti's (1830–1894) | Public domain, by the author's release | `e3b966e771d23e45964fef4565cf498f2552771f6f025b40c15697c57e72b17d` |
| `noise.opus` | [File:Urban Street on a Rainy Afternoon.flac](https://commons.wikimedia.org/wiki/File:Urban_Street_on_a_Rainy_Afternoon.flac), seconds 600–612 | Extemporalist, "Own work" | [CC0 1.0](https://creativecommons.org/publicdomain/zero/1.0/) | `ef0511593b1bb0ba47d5c5dafd83eb07ba854b5a65fd5d41b8c76a1b3ff2c22a` |

A street in Prague was considered for the noise and left out, because people talk in it: the tests
need noise with no voice in it.

## How they were made

From the downloads, each in a directory of its own, with ffmpeg 9.0.2:

```sh
ffmpeg -ss 20  -t 12 -i prof.ogg   -ac 1 -ar 48000 -af volume=19dB -f s16le near.raw
ffmpeg -ss 4   -t 12 -i sonnet.ogg -ac 1 -ar 48000 -af volume=-2dB -f s16le far.raw
ffmpeg -ss 600 -t 12 -i rain.flac  -ac 1 -ar 48000 -af volume=33dB -f s16le noise.raw
```

Then, from `voiceworker/`, for each of the three:

```sh
go run ./internal/audio/audiotest/recordings/gen.go <near.raw >internal/audio/audiotest/recordings/testdata/near.opus
```

ffmpeg's resampling is not promised to give the same bytes in another version, so the packets are the
record, not these commands.
