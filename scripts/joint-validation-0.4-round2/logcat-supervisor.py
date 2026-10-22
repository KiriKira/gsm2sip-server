import pathlib,subprocess,time,sys
root=pathlib.Path('/home/ubuntu/gsm-webrtc-validation/joint-0.4');adb='/home/ubuntu/gsm-webrtc-validation/android-sdk/platform-tools/adb';role=sys.argv[1];assert role in ['host','gateway'];serial='emulator-5556' if role=='host' else 'emulator-5554'
while True:
 subprocess.run([adb,'-s',serial,'wait-for-device'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
 with (root/'private'/(role+'-logcat.txt')).open('ab') as out:subprocess.run([adb,'-s',serial,'logcat','-v','threadtime','-T','1'],stdout=out,stderr=out)
 time.sleep(1)
