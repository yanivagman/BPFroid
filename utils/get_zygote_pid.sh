#!/bin/sh

ZYGOTE_PID=`adb shell ps -u 0 | grep zygote | awk '{print $2}'`
echo $ZYGOTE_PID
