# Recordings

The GIFs here are rendered from the asciicast files in `casts/`, which carry
real output from a real gateway: the streaming one plays back at the speed the
provider actually answered, and the fallback one is a genuine primary going
down.

To re-render after editing a cast:

```bash
agg --font-size 17 --theme asciinema --idle-time-limit 2 casts/streaming.cast streaming.gif
```

[`agg`](https://github.com/asciinema/agg) is asciinema's GIF renderer. Keep the
content inside the terminal's row count: a recording that scrolls forces a full
redraw on every frame, which took one of these from 45 KB to 1.7 MB.
