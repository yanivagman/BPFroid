#!/bin/sh

adb push dist/tracee /data/local/tmp/tracee && adb push dist/tracee.bpf.o /data/local/tmp/
