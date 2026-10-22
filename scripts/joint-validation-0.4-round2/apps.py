"""Install matching local builds in place, preserving package data and pairing."""
import importlib.util,json,pathlib,subprocess,time,sys
root=pathlib.Path('/home/ubuntu/gsm-webrtc-validation/joint-0.4');ev=root/'evidence';private=root/'private'
spec=importlib.util.spec_from_file_location('driver',root/'scripts/driver.py');d=importlib.util.module_from_spec(spec);spec.loader.exec_module(d)
def install(stage,role):
 assert stage in ('baseline','candidate') and role in d.SERIALS
 serial=d.SERIALS[role];pkg=d.PACKAGES[role];d.ADB[-1]=serial
 assert d.adb('shell','getprop','sys.boot_completed').strip()=='1'
 fixture=d.read(role,'joint-fixture.json');assert fixture,'retained_fixture_missing';fixture['reuse_pairing']=True;d.write(role,'joint-fixture.json',fixture)
 subprocess.run([d.ADB[0],'-s',serial,'shell','am','force-stop',pkg],check=True)
 for name in [role+'-debug.apk',role+'-debug-androidTest.apk']:
  file=root/'builds'/stage/role/name
  p=subprocess.run([d.ADB[0],'-s',serial,'install','-r','-g',str(file)],capture_output=True,text=True,timeout=180)
  assert p.returncode==0 and 'Success' in p.stdout,'inplace_install_failed:'+p.stdout+p.stderr
 p=subprocess.run([d.ADB[0],'-s',serial,'shell','am','start','-W','-n',pkg+'/.MainActivity'],capture_output=True,text=True,timeout=60)
 assert p.returncode==0 and 'Status: ok' in p.stdout,'main_activity_start_failed:'+p.stdout+p.stderr
 dump=d.adb('shell','dumpsys','package',pkg)
 versions=[l.strip() for l in dump.splitlines() if 'versionCode=' in l or 'versionName=' in l]
 assert any('versionCode=1000000000 ' in l for l in versions),'wrong_code'
 expected_names={'versionName=0.3.0-joint-baseline'} if stage=='baseline' else {'versionName=0.4.0-joint','versionName=0.4.0-joint-round2'}
 assert any(l in expected_names for l in versions),'wrong_name'
 r={'status':'PASS','stage':stage,'role':role,'inplace_install':True,'main_activity_started':True,'versions':versions,'fixture_retained':True,'utc':time.time()}
 (ev/('install-'+role+'-'+stage+'-'+str(int(time.time()))+'.json')).write_text(json.dumps(r,indent=2)+'\n');print(json.dumps(r),flush=True)
def start(role):
 d.ADB[-1]=d.SERIALS[role];fixture=d.read(role,'joint-fixture.json');assert fixture;fixture['reuse_pairing']=True;d.write(role,'joint-fixture.json',fixture)
 for args in [('shell','svc','power','stayon','true'),('shell','input','keyevent','KEYCODE_WAKEUP'),('shell','wm','dismiss-keyguard'),('shell','cmd','statusbar','collapse')]:d.adb(*args)
 d.adb('shell','run-as',d.PACKAGES[role],'rm','-f',*[f'files/joint-{n}.json' for n in ('ready','finished','command','result')])
 runner='GatewayJointInstrumentation' if role=='gateway' else 'HostJointInstrumentation'
 log=private/(role+'-instrumentation-'+str(int(time.time()))+'.log')
 with log.open('w') as f:subprocess.Popen([d.ADB[0],'-s',d.SERIALS[role],'shell','am','instrument','-w',d.PACKAGES[role]+'.test/com.callagent.lab.'+runner],stdin=subprocess.DEVNULL,stdout=f,stderr=subprocess.STDOUT,start_new_session=True)
 end=time.monotonic()+120
 while True:
  ready=d.read(role,'joint-ready.json')
  if ready and ready.get('status')=='READY':print(role,json.dumps(ready),flush=True);return
  finished=d.read(role,'joint-finished.json');assert not finished,'runner_failed:'+str(finished)
  assert time.monotonic()<end,'runner_readiness_timeout';time.sleep(2)
if __name__=='__main__':
 if sys.argv[1]=='install':install(sys.argv[2],sys.argv[3])
 elif sys.argv[1]=='start':start(sys.argv[2])
