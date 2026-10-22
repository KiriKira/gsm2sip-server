#!/usr/bin/env python3
"""Private emulator controller. No fixture secrets are printed."""
import json, subprocess, time, uuid, sys, shlex, os
from pathlib import Path
ROOT=Path('/home/ubuntu/gsm-webrtc-validation')
PRIVATE=ROOT/'virtual-lab/private'
ADB=[str(ROOT/'android-sdk/platform-tools/adb'),'-s','emulator-5554']
SERIALS={'gateway':'emulator-5554','host':os.environ.get('RTC_HOST_SERIAL','emulator-5556')}
PACKAGES={'gateway':'com.callagent.gateway','host':'com.callagent.host'}
def adb(*args,data=None):
 p=subprocess.run(ADB+list(args),input=data,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True,timeout=45)
 if p.returncode:raise RuntimeError('adb_failed:'+p.stderr[-250:])
 return p.stdout

def read(role,name):
 ADB[-1]=SERIALS[role]
 try:return json.loads(adb('exec-out','run-as',PACKAGES[role],'cat','files/'+name))
 except (ValueError,RuntimeError):return None

def write(role,name,obj):
 ADB[-1]=SERIALS[role]
 # exec-out may keep the remote stdin open after communicate() closes its
 # local pipe. Use a quoted, finite write so the command always completes.
 script='mkdir -p files && printf %s '+shlex.quote(json.dumps(obj))+' > '+shlex.quote('files/'+name)
 adb('shell','run-as '+shlex.quote(PACKAGES[role])+' sh -c '+shlex.quote(script))

def setup(role):
 code=next(l.split('=',1)[1] for l in (PRIVATE/f'joint-{role}-code.txt').read_text().splitlines() if l.startswith('pairing_code='))
 config={'api_url':'http://10.0.2.2:18096','pairing_code':code}
 if role=='host':
  ready=read('gateway','joint-ready.json')
  assert ready and ready['status']=='READY','gateway_not_ready'
  config['gateway_id']=ready['gateway_id']
 write(role,'joint-fixture.json',config)
 print(role,'fixture ready')

def command(role,op,**args):
 job={'id':str(uuid.uuid4()),'op':op,**args}
 write(role,'joint-command.json',job)
 if op=='stop':
  print(json.dumps({'status':'SENT','role':role,'op':op}));return
 end=time.monotonic()+180
 while time.monotonic()<end:
  result=read(role,'joint-result.json')
  if result and result.get('id')==job['id']:
   print(json.dumps(result))
   if result.get('status')!='OK':raise RuntimeError('joint_command_failed')
   return result
  time.sleep(.5)
 raise RuntimeError('command_timeout')

if __name__=='__main__':
 action=sys.argv[1]
 if action=='setup':setup(sys.argv[2])
 elif action=='resume':
  role=sys.argv[2];config=read(role,'joint-fixture.json');assert config,'resume_fixture_missing';config['reuse_pairing']=True;write(role,'joint-fixture.json',config);print(role,'existing_pairing_retained')
 elif action=='read':print(json.dumps(read(sys.argv[2],sys.argv[3])))
 elif action=='command':
  options=json.loads(sys.argv[4]) if len(sys.argv)>4 else {}
  command(sys.argv[2],sys.argv[3],**options)
 elif action=='modem':
  ADB[-1]=SERIALS['gateway']
  # Emulator only. The number is fixed; no physical telephony CLI is used.
  print(adb('emu','gsm',sys.argv[2],'+15550100200'))
 else:raise ValueError('unknown action')
