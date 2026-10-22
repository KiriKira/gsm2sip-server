"""Only changes server image tags; retains volumes, DBs, keys, ports and routing."""
import pathlib,subprocess,json,time,urllib.request,sys
root=pathlib.Path('/home/ubuntu/gsm-webrtc-validation/joint-0.4');private=root/'private';ev=root/'evidence'
def docker(*a):return subprocess.check_output(['sudo','-n','docker',*a],text=True).strip()
def inspect(n):return json.loads(docker('container','inspect',n))[0]
def wait(url,candidate):
 end=time.monotonic()+90
 while True:
  try:
   with urllib.request.urlopen(url+'/v1/capabilities',timeout=3) as r:c=json.load(r)
   assert c['rtc_audio'] and c['rtc_call_control']
   if candidate:assert not any('sip' in k for k in c)
   else:assert c['sip_call_control']==False
   return c
  except Exception:
   if time.monotonic()>end:raise
   time.sleep(2)
def switch(stage):
 assert stage in ('baseline','candidate');tag='joint-0.4' if stage=='candidate' else 'joint-baseline';result={'status':'RUNNING','stage':stage,'nodes':[],'utc':time.time()}
 for node,name,ip,envfile,volume,published,url in [('A','gsm-rtc-joint','198.18.0.2','a.env','gsm-rtc-joint-data',True,'http://127.0.0.1:18098'),('C','gsm-rtc-mesh-c','10.203.78.10','c.env','gsm-rtc-mesh-c-data',False,'http://10.203.78.10:8080')]:
  current=inspect(name);old=current['Image'];docker('stop','-t','30',name)
  with (private/(name+'-pre-'+stage+'-'+str(int(time.time()))+'.log')).open('w') as f:subprocess.run(['sudo','-n','docker','logs',name],stdout=f,stderr=subprocess.STDOUT,check=True)
  docker('rm',name)
  args=['run','-d','--name',name,'--network','gsm-rtc-mesh-'+node.lower(),'--ip',ip,'--memory','640m','--cpus','1','--shm-size','32m','--restart','unless-stopped','--env-file',str(private/envfile),'--mount','type=volume,src='+volume+',dst=/data']
  if published:args+=['-p','127.0.0.1:18098:8080']
  docker(*args,'soulgem-aio:'+tag);cap=wait(url,stage=='candidate');top=docker('top',name,'-eo','pid,comm');assert 'asterisk' not in top.lower()
  result['nodes'].append({'node':node,'before':old,'after':inspect(name)['Image'],'capabilities':cap,'asterisk_processes':0})
 envpath=private/'b.env';env=envpath.read_text().splitlines();envpath.write_text('\n'.join('GSM2SIP_VERSION='+tag if l.startswith('GSM2SIP_VERSION=') else l for l in env)+'\n');envpath.chmod(0o600)
 compose=['sudo','-n',str(root/'docker-compose'),'--project-name','gsm04b','--env-file',str(envpath),'-f',str(root/'deploy/compose.yaml'),'-f',str(root/'b-override.yaml')]
 subprocess.run(compose+['up','-d'],check=True,stdout=subprocess.DEVNULL)
 cap=wait('http://10.203.78.2:8080',stage=='candidate')
 for n in ['gsm-rtc-mesh-b-api','gsm-rtc-mesh-b-worker']:assert 'asterisk' not in docker('top',n,'-eo','pid,comm').lower()
 result['nodes'].append({'node':'B','after':inspect('gsm-rtc-mesh-b-api')['Image'],'capabilities':cap,'asterisk_processes':0,'compose':'0.4 unchanged'})
 result['status']='PASS';(ev/('server-switch-'+stage+'-'+str(int(time.time()))+'.json')).write_text(json.dumps(result,indent=2)+'\n');print(json.dumps(result))
if __name__=='__main__':switch(sys.argv[1])
