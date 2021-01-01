#!/usr/bin/env python3

import os
import time
import sys

apk = "\"" + sys.argv[1] + "\""
aapt = "aapt/aapt"

# TODO: Use SIM card for good evaluation!!!!

# TODO: validate argument
# TODO: let the user choose the time to run. Default to 5 minutes
# TODO: add an option to avoid performing uninstall (to check for malwares that use time to evade detection)
# TODO: add an option to force install also if package not installed
# TODO: restore device to known state (by restoring data and system(?) partitions)

# Other usefull commands
# screencap -p /sdcard/filename.jpg
# dumpsys activity activities | grep mResumedActivity
# /system/bin/input tap x y
# Launch chrome: monkey -p com.android.chrome -c android.intent.category.LAUNCHER 1
# "pm list packages -f" shows apk to package name mapping after installation
# Clear app data: adb shell pm clear com.package.name
# https://engineering.nodesagency.com/categories/android/2019/04/29/automate-debugging-and-testing-workflows-using-adb
# Starting and stopping tests: https://github.com/idanr1986/cuckoodroid/blob/master/analyzer/android/modules/packages/apk.py

# Another way to get package name and main activity:
# https://github.com/idanr1986/cuckoodroid/blob/c2d15d1d537ef12d286546fa3804c178e8163452/lib/cuckoo/common/objects.py#L350

# checking for connected devices
#device = os.popen("adb devices").read().split('\n', 1)[1].split("device")[0].strip()

# connect to the selected device 172.0.0.1:62001
#print("Waiting for connection ...")
#connect = os.popen("adb connect " + device ).read()
#print(connect)

#start Epic application
#rc = os.system("adb shell monkey -p com.getepic.Epic -c android.intent.category.LAUNCHER 1")

def close_bpfroid(pid):
	print("Stopping BPFroid...")
	rc = os.system("adb shell su -c kill -2 " + pid + " 2> /dev/null")
	if rc != 0:
		print("failed to stop BPFroid!")
	os.system("adb shell su -c rm /data/local/tmp/out/tracee.pid 2> /dev/null")

print("apk: " + apk)
# Get apk package name
cmd = aapt + " dump badging " + apk + " | grep package: | cut -d' ' -f2 | grep -o \"'.*'\" | sed \"s/'//g\""
package_name = os.popen(cmd).read().rstrip()
print("apk package name: " + package_name)

# Get apk launcher activity name
cmd = aapt + " dump badging " + apk + " | grep launchable-activity: | cut -d' ' -f2 | grep -o \"'.*'\" | sed \"s/'//g\""
activity_name = os.popen(cmd).read().rstrip()
print("apk launcher activity: " + activity_name)

# Check if apk already installed
is_installed = os.popen("adb shell pm list packages " + package_name).read()
if is_installed == "":
	print("Installing apk...")
	rc = os.system("adb install " + apk)
	if rc != 0:
		print("failed to install apk")
		exit()
else:
	print("Package " + package_name + " already installed!")

# Start tracee with package name filter:
print("Strating BPFroid...")
os.system("adb shell su -c rm /data/local/tmp/out/tracee.pid 2> /dev/null")
bpfroid = os.popen("adb shell su -c /data/local/tmp/tracee --security-alerts -c mem -c exec -c write=memfd* -c write=/* -c clear-dir -t e=security_bprm_check -t e=vfs_write,vfs_writev -t vfs_write.pathname=/* -t vfs_write.pathname=memfd* -t package=" + package_name)
for x in range(1000):
	bpfroid_pid = os.popen("adb shell su -c cat /data/local/tmp/out/tracee.pid 2> /dev/null").read().rstrip()
	if bpfroid_pid != "":
		break

if bpfroid_pid == "":
	print("Failed to start BPFroid, aborting...")
	exit()

print("BPFroid ready (pid: " + bpfroid_pid + ")")

# Run application
rc = os.system("adb shell am start -n " + package_name + "/" + activity_name)
if rc != 0:
	print("failed to start application!")
	close_bpfroid(bpfroid_pid)
	exit()

# Wake up device (simulating power key)
os.system("adb shell input keyevent 26")
# unlock device
os.system("adb shell input keyevent 82")

# Sleep
print("\nGoing to sleep for 5 minutes... You can interact with the application during this time\n")
time.sleep(5)

print("Timeout. Killing application...")
#os.system("adb shell pm disable " + package_name)
os.system("adb shell am force-stop " + package_name)
os.system("adb uninstall " + package_name)

close_bpfroid(bpfroid_pid)

output = bpfroid.read()
print("Output from BPFroid:\n\n" + output)

