# Acknowledgments

## ah4c

Coaxial carries two small functions from [ah4c](https://github.com/sullrich/ah4c), unchanged: `copyFlush` (a copy that
pushes every piece out as soon as it is written) and `readWithDeadline` (a read that gives up on a source that has gone
quiet). They are in `coaxial/passthrough.go`, under ah4c's notice, and do the copying whenever Coaxial passes a movie, a
series episode, catch-up, the guide or a video piece from a provider to a player. The script that stamps the version
at build time, `coaxial/bump-version.sh`, is also ah4c's, unchanged apart from one added comment line.

ah4c was originally written by Aman Karmani ([tmm1](https://github.com/tmm1)), and has since been
developed and maintained by Scott Ullrich ([sullrich](https://github.com/sullrich)), Scott Ueland
([bnhf](https://github.com/bnhf)) and [mackid1993](https://github.com/mackid1993). Thank you to all four.

ah4c's license, word for word:

```
Unless noted at the top of each file, Copyright 2023 Fancy Bits, LLC

Permission is hereby granted, free of charge, to any person obtaining a copy of this software and associated documentation files (the “Software”), to deal in the Software without restriction, including without limitation the rights to use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of the Software, and to permit persons to whom the Software is furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED “AS IS”, WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
```

## gluetun

The container image is built on the stock [gluetun](https://github.com/qdm12/gluetun) image, unchanged. gluetun makes
the VPN connection and keeps everything else from leaving the container. It is the work of Quentin McGaw and is under
the MIT license.

gluetun's license, word for word:

```
MIT License

Copyright (c) 2018 Quentin McGaw

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## pro-bing

Coaxial pings the VPN servers with [pro-bing](https://github.com/prometheus-community/pro-bing), unchanged, as a Go
module (`coaxial/probe.go`). pro-bing is the ping library of the Prometheus project, kept by the Prometheus
community, and grew out of Cameron Sparr's go-ping. Thank you to its authors. It brings four small Go modules with it:
`golang.org/x/net`, `golang.org/x/sync` and `golang.org/x/sys` from the Go project (BSD-3-Clause) and
`github.com/google/uuid` (BSD-3-Clause).

pro-bing's license, word for word:

```
The MIT License (MIT)

Copyright 2022 The Prometheus Authors
Copyright 2016 Cameron Sparr and contributors.

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## zerolog

Coaxial writes its log with [zerolog](https://github.com/rs/zerolog), unchanged, as a Go module (`coaxial/logging.go`).
zerolog is the work of Olivier Poitrey and its contributors, under the MIT license. It brings two small Go modules
with it, `github.com/mattn/go-colorable` and `github.com/mattn/go-isatty`, both by Yasuhiro Matsumoto and under the MIT
license.

zerolog's license, word for word:

```
MIT License

Copyright (c) 2017 Olivier Poitrey

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## flag-icons

The country flag that the web page shows next to the VPN's exit country is from
[flag-icons](https://github.com/lipis/flag-icons) by Panayiotis Lipiridis ([lipis](https://github.com/lipis)). The
files in `coaxial/ui/flags/` are its 4x3 set, unchanged. flag-icons is under the MIT license.

flag-icons' license, word for word:

```
The MIT License (MIT)

Copyright (c) 2013 Panayiotis Lipiridis

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction, including without limitation the rights to
use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies
of the Software, and to permit persons to whom the Software is furnished to do
so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```
