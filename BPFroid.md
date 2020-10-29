# BPFroid
Trace Android framework API, native libraries, system calls and other events using eBPF.
Based on Tracee: https://github.com/aquasecurity/tracee

## Requirements
* Android linux kernel that provides BPF capabilities: BPF, Kprobes and Uprobes
* kernel headers (used for BPF program compilation)
* clang

## Building BPFroid for arm64
* Prepare compilation environment with docker:
  docker run -it --rm --privileged multiarch/qemu-user-static --credential yes --persistent yes
* Use the Dockerfile in builder directory to build an image of the build environment
* Run builder container with BPFroid sources mounted and kernel headers as well, e.g:
  docker run -it --rm -v /path/to/tracee:/tracee -v /path/to/android-kernel:/headers bpfroid_builder
* Set KERN_HEADERS variable in the Makefile to point to the correct location, then make, e.g:
  KERN_HEADERS=/headers make

## Building for android emulator
* KERN_HEADERS=/path/to/android-goldfish-kernel make

## Running BPFroid
* Clone and build BPFroid
* Copy bpfroid binary and bpf object file to target device (built into "dist" by default)
* Configure required hooks in hooks.json
* Run

## Notes
* System updates that change oat framework files requires deleting hooks.cache file!
