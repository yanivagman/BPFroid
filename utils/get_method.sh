#!/bin/sh

ZYGOTE_PID=`adb shell ps -u 0 | grep zygote | awk '{print $2}'`
OAT_PATHS=`adb shell cat /proc/$ZYGOTE_PID/maps | grep "r\-xp.*oat" | cut -d "/" -f 2- | sed 's/^/\//'`
for OAT_PATH in $OAT_PATHS; do echo $OAT_PATH; adb shell oatdump --oat-file=$OAT_PATH --class-filter=$1 --method-filter=$2 | grep -E 'dex_method_idx|code_offset:'; done

