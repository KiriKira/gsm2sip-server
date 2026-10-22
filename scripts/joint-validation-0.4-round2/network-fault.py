"""UID-scoped faults for the lab's three explicit API/TURN addresses."""
import json,pathlib,subprocess,fcntl
root=pathlib.Path('/home/ubuntu/gsm-webrtc-validation/joint-0.4');private=root/'private';chain='GSM_04_LAB'
def run(*a,check=True):return subprocess.run(['sudo','-n','iptables','-w','2',*a],check=check,stdout=subprocess.DEVNULL,stderr=subprocess.PIPE)
def apply(action):
 assert action in ('restore','a_down','a_up','b_down','b_up','c_down','c_up','relay_on','relay_off','turn_udp_down','turn_udp_up','media_down','media_up')
 with (private/'fault.lock').open('w') as lock:
  fcntl.flock(lock,fcntl.LOCK_EX);f=private/'fault-state.json';s=json.loads(f.read_text()) if f.exists() else {}
  if action=='restore':s={}
  elif action.startswith(('a_','b_','c_')):s[action[0]+'_down']=action.endswith('_down')
  elif action.startswith('relay_'):s['relay']=action.endswith('_on')
  elif action.startswith('turn_udp_'):s['turn_udp_down']=action.endswith('_down')
  elif action.startswith('media_'):s['media_down']=action.endswith('_down')
  run('-N',chain,check=False)
  if run('-C','OUTPUT','-m','owner','--uid-owner','1000','-j',chain,check=False).returncode:run('-I','OUTPUT','1','-m','owner','--uid-owner','1000','-j',chain)
  run('-F',chain)
  for node,ip,port in [('a','127.0.0.1','18096'),('b','10.203.78.2','8080'),('c','10.203.78.10','8080')]:
   if s.get(node+'_down'):run('-A',chain,'-d',ip,'-p','tcp','--dport',port,'-j','REJECT')
  for ip in ['198.18.0.1','198.18.0.2','172.31.15.170']:
   if s.get('media_down'):
    run('-A',chain,'-d',ip,'-p','udp','-j','DROP');run('-A',chain,'-d',ip,'-p','tcp','--dport','3478','-j','DROP')
   elif s.get('relay'):
    run('-A',chain,'-d',ip,'-p','udp','!','--dport','3478','-j','DROP')
   if s.get('turn_udp_down'):run('-A',chain,'-d',ip,'-p','udp','--dport','3478','-j','DROP')
  if not any(s.values()):run('-D','OUTPUT','-m','owner','--uid-owner','1000','-j',chain);run('-X',chain)
  f.write_text(json.dumps(s));return s
if __name__=='__main__':
 import sys;print(json.dumps(apply(sys.argv[1])))
