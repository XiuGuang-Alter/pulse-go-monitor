// Actual v1.4 schema4 -> current schema5; synthetic data only. Node is a test driver.
const assert=require('node:assert/strict');
const {execFileSync}=require('node:child_process');
const fs=require('node:fs/promises'),os=require('node:os'),path=require('node:path'),http=require('node:http');
const {once}=require('node:events');
const {DatabaseSync}=require('node:sqlite');
const {createHash}=require('node:crypto');
const {setTimeout:delay}=require('node:timers/promises');
const {launchServer,stopChild}=require('./launch-server.cjs');

async function main(){
 const args=process.argv.slice(2);if(args.length!==2)throw Error('Pass actual v1.4 and candidate binary paths');
 const [old,candidate]=args.map(p=>path.resolve(p));
 const dir=await fs.mkdtemp(path.join(os.tmpdir(),'pulse-event-upgrade-'));
 let app,upstream,status=503;
 const source=path.join(dir,'v14.db'),upgraded=path.join(dir,'v15.db'),backup=path.join(dir,'before.zip'),eventsBackup=path.join(dir,'events.zip'),restored=path.join(dir,'restored.db'),rollback=path.join(dir,'rollback.db');
 const launch=async(binary,db)=>{app=await launchServer(binary,['serve','-addr','127.0.0.1:0','-db',db]);};
 const stop=async()=>{await stopChild(app?.child);app=undefined;};
 const api=async(route,method='GET',body)=>{const r=await fetch(app.url+route,{method,body:body&&JSON.stringify(body),headers:{'Content-Type':'application/json'},signal:AbortSignal.timeout(3000)});assert(r.ok,`${route}: ${r.status}`);return r.json();};
 const until=async(fn)=>{for(let n=0;n<160;n++){const value=await fn();if(value)return value;await delay(50);}throw Error('Timed out waiting for fixture');};
 const maintain=(...args)=>JSON.parse(execFileSync(candidate,['maintain',...args],{encoding:'utf8',windowsHide:true,timeout:130000}));
 const schema=file=>{const db=new DatabaseSync(file,{readOnly:true});try{return db.prepare('PRAGMA user_version').get().user_version;}finally{db.close();}};
 const hash=async file=>createHash('sha256').update(await fs.readFile(file)).digest('hex');
 try{
  upstream=http.createServer((req,res)=>res.writeHead(status).end('synthetic event fixture'));upstream.listen(0,'127.0.0.1');await once(upstream,'listening');
  const input={name:'event upgrade fixture',url:`http://127.0.0.1:${upstream.address().port}/?token=synthetic-private`,interval_seconds:60,timeout_seconds:1,enabled:false};
  await launch(old,source);const target=await api('/api/targets','POST',input);
  await api(`/api/targets/${target.id}/check`,'POST');
  const original=await until(async()=> (await api(`/api/targets/${target.id}/checks`)).items[0]);
  await stop();assert.equal(schema(source),4);
  const originalHash=await hash(source);const before=maintain('backup','-db',source,'-out',backup);assert.equal(before.schema_version,4);maintain('verify','-in',backup);maintain('restore','-in',backup,'-db',upgraded);
  assert.equal(await hash(source),originalHash);
  await launch(candidate,upgraded);assert.deepEqual(await api('/api/events'),[],'migration fabricated events');
  const history=await api(`/api/targets/${target.id}/checks`);assert.deepEqual(history.items[0],original);assert.equal((await api('/api/targets'))[0].id,target.id);
  await api(`/api/targets/${target.id}`,'PUT',{...input,enabled:true});
  await until(async()=> (await api(`/api/targets/${target.id}/checks`)).items.length===2);
  await until(async()=> !(await api('/api/targets'))[0].running);
  await api(`/api/targets/${target.id}/check`,'POST');
  await until(async()=> (await api('/api/events')).length===1);
  status=200;await until(async()=> !(await api('/api/targets'))[0].running);await api(`/api/targets/${target.id}/check`,'POST');
  const events=await until(async()=> {const e=await api('/api/events');return e.length===2&&e;});
  assert.deepEqual(events.map(e=>e.kind),['recovery','fault']);assert(events.every(e=>e.notification_status==='disabled'));assert(!JSON.stringify(events).includes('synthetic-private'));
  await api(`/api/targets/${target.id}`,'PUT',{...input,enabled:false});await stop();assert.equal(schema(upgraded),5);
  const after=maintain('backup','-db',upgraded,'-out',eventsBackup);assert.equal(after.counts.alert_events,2);maintain('verify','-in',eventsBackup);maintain('restore','-in',eventsBackup,'-db',restored);
  await launch(candidate,restored);assert.deepEqual(await api('/api/events'),events);await stop();
  let refused=false;try{await launch(old,upgraded);}catch(error){refused=/版本|version/i.test(String(error));}assert(refused,'v1.4 accepted schema5');
  maintain('restore','-in',backup,'-db',rollback);assert.equal(schema(rollback),4);await launch(old,rollback);assert.deepEqual((await api(`/api/targets/${target.id}/checks`)).items[0],original);await stop();
  assert.equal(await hash(source),originalHash);
  console.log(JSON.stringify({passed:true,old_binary_sha256:await hash(old),new_binary_sha256:await hash(candidate),old_schema:4,new_schema:5,no_backfill:true,target_and_history_preserved:true,events_preserved_by_backup_restore:true,desktop_disabled_faults:1,desktop_disabled_recoveries:1,old_binary_refused_upgraded:true,restored_preupgrade_backup_opened_by_v14:true,original_db_unchanged:true}));
 }finally{await stop();if(upstream)await new Promise(r=>upstream.close(r));await fs.rm(dir,{recursive:true,force:true});}
}
main().catch(error=>{console.error(error);process.exitCode=1;});
