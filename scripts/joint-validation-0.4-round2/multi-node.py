import importlib.util,pathlib,json,time,subprocess,sys
root=pathlib.Path('/home/ubuntu/gsm-webrtc-validation/joint-0.4')
def mod(name):
 s=importlib.util.spec_from_file_location(name,root/'scripts'/(name+'.py'));m=importlib.util.module_from_spec(s);s.loader.exec_module(m);return m
c=mod('core-calls');fault=mod('network-fault');urls={'a':'http://10.0.2.2:18096/v1','b':'http://10.203.78.2:8080/v1','c':'http://10.203.78.10:8080/v1'}
def select(case,node,timeout=150):
 c.d.ADB[-1]=c.d.SERIALS['host'];c.d.adb('shell','am','start','-n','com.callagent.host/.MainActivity','-f','0x34000000')
 end=time.monotonic()+timeout
 while True:
  h,g=case.sample()
  if h.get('active_node')==urls[node] and h.get('listening') and any(n.get('active') and 'callReady=true' in n.get('health','') for n in h.get('node_health',[])):return h,g
  assert time.monotonic()<end,'automatic_node_selection_timeout:'+node;time.sleep(4)
def run(mode):
 case=c.Case(mode)
 try:
  fault.apply('restore');select(case,'a')
  if mode=='incoming-partial-node-unreachable':
   # DROP keeps B's last-ready observation through the initial 3-second
   # request deadline, exercising a failed create rather than omitting B.
   subprocess.run(['sudo','-n','iptables','-w','2','-N','GSM_04_FORK'],check=True)
   subprocess.run(['sudo','-n','iptables','-w','2','-A','GSM_04_FORK','-d','10.203.78.2','-p','tcp','--dport','8080','-j','DROP'],check=True)
   subprocess.run(['sudo','-n','iptables','-w','2','-I','OUTPUT','1','-m','owner','--uid-owner','1000','-j','GSM_04_FORK'],check=True)
   case.incoming()
   # Ensure C received an invitation before answering A, then inspect the
   # loser's terminal in its own independent DB.
   call=case.result['call_id'];end=time.monotonic()+8
   while True:
    state=c.sql("SELECT state FROM call_sessions WHERE call_id='"+call+"'",'c')
    if state=='ringing':break
    assert time.monotonic()<end,'node_c_not_offered';time.sleep(.3)
   c.cmd('host','answer');case.connected();assert case.result['active_node']==urls['a'];case.tones(12)
   end=time.monotonic()+20
   while True:
    loser=c.sql("SELECT c.state,coalesce(c.reason,''),p.state,coalesce(p.reason,'') FROM call_sessions c JOIN call_participants p ON p.call_id=c.call_id WHERE c.call_id='"+call+"'",'c')
    if 'answered_elsewhere' in loser and loser.startswith('ended|'):break
    assert time.monotonic()<end,'node_c_not_answered_elsewhere';time.sleep(1)
   case.result['node_c_terminal']=loser
   text=(root/'private/gateway-logcat.txt').read_text(errors='replace');assert any('call_id='+call in l and 'event=incoming_offer_failed' in l for l in text.splitlines()),'failed_B_create_not_exercised'
   case.result['B_failed_create_logged']=True;case.end('host')
  elif mode=='idle-failover-hold-failback':
   fault.apply('a_down');select(case,'b');case.outgoing();assert case.result['active_node']==urls['b'];case.tones(12);fault.apply('a_up');started=time.monotonic()
   while True:
    time.sleep(6);h,g=case.active();assert h.get('active_node')==urls['b'],'active_call_migrated'
    a=next(n for n in h['node_health'] if n['api_url']==urls['a'])
    import re
    successes=re.search(r'consecutiveSuccesses=(\d+)',a.get('health',''))
    if successes and int(successes[1])>=3:break
    assert time.monotonic()-started<120,'primary_not_requalified'
   case.result['B_held_while_A_healthy']='PASS';case.end('host');select(case,'a',timeout=90);case.result['idle_failback']='PASS'
  else:raise ValueError(mode)
  case.pass_()
 except Exception as error:case.result.update(status='FAIL',error=str(error));case.save();raise
 finally:
  for args in [('-D','OUTPUT','-m','owner','--uid-owner','1000','-j','GSM_04_FORK'),('-F','GSM_04_FORK'),('-X','GSM_04_FORK')]:subprocess.run(['sudo','-n','iptables','-w','2',*args],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
  fault.apply('restore')
if __name__=='__main__':run(sys.argv[1])
