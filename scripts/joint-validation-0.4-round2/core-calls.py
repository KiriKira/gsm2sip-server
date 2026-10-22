"""0.4 real service/Telecom/RTC calls; only hardware PCM is synthetic."""
import contextlib,datetime,importlib.util,io,json,os,pathlib,subprocess,time,uuid,sqlite3,tarfile,sys,threading
root=pathlib.Path('/home/ubuntu/gsm-webrtc-validation/joint-0.4');ev=root/'evidence';private=root/'private'
spec=importlib.util.spec_from_file_location('driver',root/'scripts/driver.py');d=importlib.util.module_from_spec(spec);spec.loader.exec_module(d)
def cmd(role,op,**kw):
 with (private/'driver.log').open('a') as log,contextlib.redirect_stdout(log):return d.command(role,op,**kw)
def sql(q,node='a'):
 if node=='b':args=['exec','gsm-rtc-mesh-b-db','psql','-X','-U','gsm2sip','-d','gsm2sip','-At','-c',q]
 else:args=['exec','--user','gsm2sip','gsm-rtc-joint' if node=='a' else 'gsm-rtc-mesh-c','/usr/lib/postgresql/17/bin/psql','-X','-h','/run/postgresql','-p','25432' if node=='a' else '55432','-U','gsm2sip','-d','gsm2sip','-At','-c',q]
 return subprocess.check_output(['sudo','-n','docker',*args],text=True).strip()
def server_node(url):return 'b' if '10.203.78.2:' in (url or '') else 'c' if '10.203.78.10:' in (url or '') else 'a'

def gateway_ledger(call):
 assert str(uuid.UUID(call))==call
 data=subprocess.check_output([d.ADB[0],'-s',d.SERIALS['gateway'],'exec-out','run-as',d.PACKAGES['gateway'],'tar','-cf','-','databases'])
 dest=private/('ledger-'+call);dest.mkdir(exist_ok=True)
 with tarfile.open(fileobj=io.BytesIO(data)) as t:t.extractall(dest,filter='data')
 con=sqlite3.connect(dest/'databases/gateway-data.db');rows=con.execute('select call_id,state,direction from call_ledger where call_id=?',(call,)).fetchall();slot=con.execute('select count(*) from rtc_cellular_slot where call_id=?',(call,)).fetchone()[0];con.close()
 return {'rows':rows,'occupied':slot}
class Case:
 def __init__(self,name):self.name=name;self.result={'case':name,'status':'RUNNING','samples':[],'utc':datetime.datetime.now(datetime.timezone.utc).isoformat()};self.path=ev/(name+'.json');self.save()
 def save(self):self.path.write_text(json.dumps(self.result,indent=2)+'\n')
 def sample(self):
  h,g=cmd('host','status'),cmd('gateway','status');self.result['samples'].append({'elapsed':time.monotonic()-self.start if hasattr(self,'start') else 0,'host':h,'gateway':g});self.save();return h,g
 def wait(self,pred,timeout=65):
  end=time.monotonic()+timeout
  while True:
   h,g=self.sample()
   if pred(h,g):return h,g
   assert time.monotonic()<end,'call_state_timeout';time.sleep(2)
 def idle(self):
  log=private/'gateway-logcat.txt';assert log.exists(),'gateway_log_capture_missing';self.result['gateway_log_offset']=log.stat().st_size
  h,g=self.sample();assert h.get('phase') in (None,'ENDED','FAILED') and g.get('bridge')=='IDLE','previous_call_not_idle';cmd('host','clear');return self.sample()
 def incoming(self):
  bh,bg=self.idle();d.ADB[-1]=d.SERIALS['gateway'];triggered=time.monotonic();r=d.adb('emu','gsm','call','+15550100200');assert 'OK' in r and 'KO' not in r
  self.result['modem_call_list_after_trigger']=d.adb('emu','gsm','list').strip()
  try:h,g=self.wait(lambda h,g:h.get('phase')=='INCOMING_RINGING',timeout=20)
  except Exception:
   # `adb emu gsm call` can return OK without a Telecom/RIL call reaching the
   # device. Preserve the emulator's own call list alongside the last status
   # samples so a harness failure is distinguishable from an RTC fork failure.
   self.result['modem_call_list_on_ring_timeout']=d.adb('emu','gsm','list').strip();self.save();raise
  self.result['ring_latency_seconds']=time.monotonic()-triggered
  self.result['call_id']=h['call_id'];self.result['active_node']=h.get('active_node');assert len(h['pcm'])==len(bh['pcm']) and len(g['pcm'])==len(bg['pcm']),'pcm_before_answer'
  self.result['pcm_before_answer']=0;return h,g
 def outgoing(self):
  # A previous notification action can leave the shade owning window focus.
  # Restore the visible user setup before the fixture taps the real dial UI.
  d.ADB[-1]=d.SERIALS['host'];d.adb('shell','input','keyevent','KEYCODE_WAKEUP');d.adb('shell','cmd','statusbar','collapse')
  self.idle()
  def accept():
   end=time.monotonic()+60
   while time.monotonic()<end:
    listing=subprocess.check_output([d.ADB[0],'-s',d.SERIALS['gateway'],'emu','gsm','list'],text=True,timeout=10)
    rows=[l for l in listing.splitlines() if 'outbound' in l.lower() and '15550100200' in l]
    if rows:
     self.result['outbound_modem_count']=len(rows);self.save()
     assert len(rows)==1,'duplicate_virtual_outgoing_call'
     if not any('active' in l.lower() for l in rows):
      reply=subprocess.check_output([d.ADB[0],'-s',d.SERIALS['gateway'],'emu','gsm','accept','+15550100200'],text=True,timeout=10)
      assert 'OK' in reply and 'KO' not in reply,'virtual_modem_accept_failed'
     return
    time.sleep(.3)
  self.accept_thread=threading.Thread(target=accept,daemon=True);self.accept_thread.start()
  end=time.monotonic()+20
  settle=time.monotonic()+120
  dial_started=time.monotonic()
  while True:
   try:cmd('host','dial',number='15550100200');break
   except RuntimeError:
    reply=d.read('host','joint-result.json') or {}
    error=str(reply.get('error',''))
    # A dial_disabled rejection means the fixture refused to dispatch, so no
    # call exists yet. "正在检查服务器通话能力" is the short in-app capability
    # probe; an offline-gateway rejection means the active node has not yet
    # seen the Gateway re-register after a failover, which the product needs
    # up to its 30 s control cadence for. Both wait and re-check; neither
    # dispatches a call, and both stay bounded.
    assert error.startswith('IllegalStateException:dial_disabled:'),error
    h,g=self.sample();assert h.get('phase') is None and g.get('bridge')=='IDLE','disabled_tap_dispatched_call'
    self.result.setdefault('dial_disabled_reasons',[]).append(error.split('dial_disabled:',1)[1])
    self.save()
    if '正在检查服务器通话能力' in error:assert time.monotonic()<end,'foreground_capability_check_timeout'
    else:assert time.monotonic()<settle,'gateway_not_call_ready_on_active_node'
    time.sleep(2)
  self.result['dial_ready_seconds']=time.monotonic()-dial_started
  return self.connected()
 def connected(self):
  h,g=self.wait(lambda h,g:h.get('phase') in ('ACTIVE','ENDED','FAILED') and g.get('bridge') in ('BRIDGED','IDLE'))
  assert h.get('phase')=='ACTIVE' and g.get('bridge')=='BRIDGED','failed_before_active';self.result['call_id']=h['call_id'];self.result['active_node']=h.get('active_node');self.start=time.monotonic();assert h.get('media')=='WebRTC' and g.get('media')=='webrtc';return h,g
 def active(self):
  h,g=self.sample();assert h.get('phase')=='ACTIVE' and g.get('bridge')=='BRIDGED' and h.get('call_id')==self.result['call_id'],'active_call_dropped_or_changed';return h,g
 def tones(self,seconds):
  beforeh,beforeg=self.active();started=time.monotonic()
  while time.monotonic()-started<seconds:time.sleep(min(8,max(.1,seconds-(time.monotonic()-started))));h,g=self.active()
  for role,b,s in [('host',beforeh,h),('gateway',beforeg,g)]:assert s['pcm'][-1]['remote_tone_frames']-b['pcm'][-1]['remote_tone_frames']>seconds*5,role+'_remote_tone_not_growing'
  self.result['audio_seconds']=time.monotonic()-started;self.save()
 def mute(self,role):
  # Both controls silence the agent -> cellular caller stream. Host mute
  # suppresses its microphone; Gateway setAgentMuted gates modem playback.
  # Gateway's cellular -> Host stream must keep flowing in either case.
  other='gateway';unaffected='host';cmd(role,'mute',enabled=True);time.sleep(4);bh,bg=self.active();before={'host':bh,'gateway':bg};time.sleep(12);h,g=self.active();after={'host':h,'gateway':g}
  assert after[other]['pcm'][-1]['remote_tone_frames']-before[other]['pcm'][-1]['remote_tone_frames']<=3,role+'_mute_leaked'
  assert after[other]['pcm'][-1]['silent_frames']-before[other]['pcm'][-1]['silent_frames']>20,role+'_mute_no_silence'
  assert after[unaffected]['pcm'][-1]['remote_tone_frames']-before[unaffected]['pcm'][-1]['remote_tone_frames']>20,role+'_mute_reverse_stopped'
  cmd(role,'mute',enabled=False);time.sleep(10);h,g=self.active();new={'host':h,'gateway':g};assert new[other]['pcm'][-1]['remote_tone_frames']-after[other]['pcm'][-1]['remote_tone_frames']>20,role+'_unmute_failed';self.result[role+'_mute_unmute']='PASS';self.save()
 def dtmf(self):
  _,g=self.active();before=g['dtmf_events'];cmd('host','dtmf');end=time.monotonic()+20
  while True:
   _,g=self.active();new=[x for x in g['dtmf_events'] if x not in before]
   if any('DTMF tone stopped' in x for x in new):assert sum('DTMF received and played' in x for x in new)==1;break
   assert time.monotonic()<end,'dtmf_timeout';time.sleep(2)
  self.result['dtmf']='PASS'
 def end(self,how='host',completed=True):
  deadline=time.monotonic()+40
  if how in ('host','gateway'):cmd(how,'hangup')
  elif how=='modem':d.ADB[-1]=d.SERIALS['gateway'];d.adb('emu','gsm','cancel','+15550100200')
  elif how=='reject':cmd('host','reject')
  h,g=self.wait(lambda h,g:h.get('phase') in ('ENDED','FAILED') and g.get('bridge')=='IDLE' and g.get('telecom_state') in (0,7),timeout=max(0,deadline-time.monotonic()))
  call=self.result['call_id']
  while True:
   self.result['server_terminal']=sql("select state,coalesce(reason,''),control_transport,exists(select 1 from gateway_call_slots s where s.call_id=c.call_id) from call_sessions c where call_id='"+call+"'",server_node(self.result.get("active_node")))
   if self.result['server_terminal'].startswith('ended|') and self.result['server_terminal'].endswith('|rtc|f'):break
   assert time.monotonic()<deadline,'server_terminal_timeout';time.sleep(.5)
  if completed:assert h.get('phase')=='ENDED','normal_hangup_reported_as_call_failure'
  ledger=gateway_ledger(call);self.result['gateway_ledger']=ledger
  assert len(ledger['rows'])==1 and not ledger['occupied'],'ledger_duplicate_or_occupied'
  if completed:assert ledger['rows'][0][1]=='completed','ledger_not_completed'
  assert self.result['server_terminal'].startswith('ended|'),'server_not_ended'
  assert self.result['server_terminal'].endswith('|rtc|f'),'server_transport_or_slot_bad'
  log=private/'gateway-logcat.txt'
  if log.exists():
   with log.open('rb') as f:f.seek(self.result['gateway_log_offset']);tail=f.read().decode(errors='replace')
   self.result['actual_telecom_calls']=sum('GsmCallService' in l and 'Call added:' in l for l in tail.splitlines())
   assert self.result['actual_telecom_calls']==1,'duplicate_or_missing_actual_telecom_call'
  self.result['hangup_origin']=how;self.save()
 def pass_(self):
  # The evidence file is the completion record; write it before any terminal
  # output. A reader that closed stdout (pipe/tee/head, or a runner that
  # stops reading) must never turn a finished case into FAIL.
  self.result['status']='PASS';self.save()
  try:print(json.dumps({k:v for k,v in self.result.items() if k!='samples'}),flush=True)
  except (BrokenPipeError,OSError):
   try:os.dup2(os.open(os.devnull,os.O_WRONLY),sys.stdout.fileno())
   except OSError:pass
def run(mode):
 c=Case(mode)
 try:
  if mode=='outgoing-95s-controls':c.outgoing();c.tones(95);c.mute('host');c.mute('gateway');c.dtmf();c.end('host')
  elif mode=='incoming-95s-controls':c.incoming();cmd('host','answer');c.connected();c.tones(95);c.mute('host');c.mute('gateway');c.dtmf();c.end('gateway')
  elif mode=='incoming-caller-hangup':c.incoming();cmd('host','answer');c.connected();c.tones(10);c.end('modem')
  elif mode=='incoming-reject':c.incoming();c.end('reject',completed=False)
  elif mode=='incoming-caller-cancel':c.incoming();c.end('modem',completed=False)
  elif mode.startswith('quick-outgoing'):c.outgoing();c.tones(12);c.end('host')
  elif mode.startswith('quick-incoming'):c.incoming();cmd('host','answer');c.connected();c.tones(12);c.end('host')
  else:raise ValueError(mode)
  c.pass_()
 except Exception as e:c.result.update(status='FAIL',error=str(e));c.save();raise
if __name__=='__main__':run(sys.argv[1])
