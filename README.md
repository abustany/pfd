# PFD - Parallel file downloader

[![Build Status](https://travis-ci.org/abustany/pfd.svg?branch=master)](https://travis-ci.org/abustany/pfd)

PFD is a simple "download accelerator" written in Go. It (potentially)
accelerates downloads by establishing multiple connections to the upstream
server, sharding the download across them. This only works for servers accepting
range requests.

PFD is at this point bare bones, and does not support fancy features like rate
limiting. It does however support HTTP Basic Authentication, unlike for example
[axel](https://github.com/eribertomota/axel) (which is otherwise an awesome
download accelerator).

## Installing

Run `go get github.com/abustany/pfd`, and copy the `pfd` binary somwhere in your
`PATH`.

## Usage

Run `pfd -help` to see the usage instructions, and available options.

## But there are already many other download accelerators!

Yes, I know... I just felt like coding a small project in Go.
