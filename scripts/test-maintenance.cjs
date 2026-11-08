// Exercises the standalone program; development-only Node is the test driver.
// All databases, raw synthetic values and backup files stay in a private temp dir.
const fs=require('node:fs/promises'),os=require('node:os'),path=require('node:path'),http=require('node:http');
const {spawnSync}=require('node:child_process');
const {once}=require('node:events');
const {setTimeout:delay}=require('node:timers/promises');
const {createHash}=require('node:crypto');
const {DatabaseSync}=require('node:sqlite');
const {launchServer,stopChild}=require('./launch-server.cjs');

async function main(){
 const [candidateArg,oldArg]=process.argv.slice(2);
 if(!candidateArg)throw Error('Pass candidate binary and optional v1.1 binary');
 const candidate=path.resolve(candidateArg),old=oldArg?path.resolve(oldArg):candidate;
 const dir=await fs.mkdtemp(path.join(os.tmpdir(),'pulse-maintenance-'));
 let app,upstream;
 const location=name=>path.join(dir,name);
 const launch=(binary,db)=>launchServer(binary,['serve','-addr','127.0.0.1:0','-db',db]);
 const command=(args,success=true)=>{
  const result=spawnSync(candidate,['maintain',...args],{encoding:'utf8',timeout:130000,windowsHide:true,cwd:dir});
  if(result.error || (success?result.status!==0:result.status===0))throw Error('Maintenance command unexpected exit: '+args[0]+' '+result.status+' '+result.stderr);
  return result.stdout?JSON.parse(result.stdout):null;
 };
 const api=async(route,method='GET',data)=>{
  const response=await fetch(app.url+route,{method,headers:{'Content-Type':'application/json'},body:data?JSON.stringify(data):undefined});
  if(!response.ok)throw Error('Fixture API failed: '+response.status);
  return response.json();
 };
 const schema=file=>{const db=new DatabaseSync(file,{readOnly:true});try{return db.prepare('PRAGMA user_version').get().user_version;}finally{db.close();}};
 try{
  upstream=http.createServer((req,res)=>res.end('synthetic maintenance response'));
  upstream.listen(0,'127.0.0.1');await once(upstream,'listening');
  const source=location('source.db'),archive=location('backup.zip'),restored=location('restored.db');
  const rawURL='http://127.0.0.1:'+upstream.address().port+'/ok?token=fake-maintenance-secret&sig=a%2Bb';
  app=await launch(old,source);
  const target=await api('/api/targets','POST',{name:'maintenance-fixture',url:rawURL,enabled:false,interval_seconds:60,timeout_seconds:1,rules:{json_equals:[]}});
  await api('/api/targets/'+target.id+'/check','POST');
  let before;
  for(let i=0;i<100;i++){before=await api('/api/targets/'+target.id+'/checks');if(before.items.length)break;await delay(50);}
  if(before.items.length!==1)throw Error('Fixture did not persist');
  await stopChild(app.child);const sourceSchema=schema(source);
  const manifest=command(['backup','-db',source,'-out',archive]);
  if(manifest.schema_version!==sourceSchema || !manifest.contains_raw_data || manifest.counts.checks!==1)throw Error('Incorrect backup manifest');
  const backupHash=createHash('sha256').update(await fs.readFile(archive)).digest('hex');
  command(['verify','-in',archive]);command(['restore','-in',archive,'-db',restored]);
  if(schema(source)!==sourceSchema || schema(restored)!==sourceSchema)throw Error('Read/restore unexpectedly migrated');
  app=await launch(old,restored);
  const rollback=await api('/api/targets/'+target.id+'/checks');
  if(rollback.items[0].id!==before.items[0].id)throw Error('Restore lost history identity');
  await stopChild(app.child);
  app=await launch(candidate,source);await stopChild(app.child);
  if(schema(source)!==4)throw Error('Candidate did not migrate');
  if(oldArg){let refused=false;try{app=await launch(old,source);}catch{refused=true;}if(!refused)throw Error('v1.1 accepted upgraded schema4');}
  const redacted=location('redacted.json'),full=location('full.json'),imported=location('imported.db');
  command(['export-config','-db',source,'-out',redacted]);
  const safe=await fs.readFile(redacted,'utf8');if(safe.includes('fake-maintenance-secret') || !JSON.parse(safe).redacted)throw Error('Default config export not redacted');
  command(['import-config','-in',redacted,'-db',imported],false);
  command(['export-config','-db',source,'-out',full,'-full']);
  const preview=command(['import-config','-in',full,'-db',imported,'-dry-run']);
  if(!preview.dry_run || preview.source_targets!==1)throw Error('Missing import preview');
  try{await fs.stat(imported);throw Error('Dry run created a database');}catch(error){if(error.code!=='ENOENT')throw error;}
  const importedReport=command(['import-config','-in',full,'-db',imported]);
  if(importedReport.enabled_targets!==0)throw Error('Import unexpectedly enabled targets');
  app=await launch(candidate,imported);const importedTargets=await api('/api/targets');
  if(importedTargets.length!==1 || importedTargets[0].url!==rawURL || importedTargets[0].enabled || importedTargets[0].stats.samples!==0)throw Error('Config migration changed semantics');
  await stopChild(app.child);
  const jsonOut=location('history.json'),csvOut=location('history.csv');
  const historyArgs=['-db',source,'-target',String(target.id),'-from','2000-01-01T00:00:00Z','-to','2100-01-01T00:00:00Z'];
  command(['export-history',...historyArgs,'-out',jsonOut]);command(['export-history',...historyArgs,'-out',csvOut,'-format','csv']);
  const jsonText=await fs.readFile(jsonOut,'utf8'),csvText=await fs.readFile(csvOut,'utf8');
  if(JSON.parse(jsonText).checks.length!==1 || jsonText.includes('fake-maintenance-secret') || csvText.includes('fake-maintenance-secret') || !csvText.includes('metadata'))throw Error('History export missing data/safety/range');
  command(['backup','-db',source,'-out',archive],false);command(['restore','-in',archive,'-db',source],false);
  const afterHash=createHash('sha256').update(await fs.readFile(archive)).digest('hex');
  if(afterHash!==backupHash)throw Error('Backup overwritten');
  console.log(JSON.stringify({passed:true,source_schema:sourceSchema,readonly_source_preserved:true,backup_verified:true,restored_schema_preserved:true,old_binary_rollback_verified:!!oldArg,default_export_redacted:true,dry_run_no_database:true,import_paused:true,history_json_csv_safe:true,existing_targets_unchanged:true}));
 }finally{
  await stopChild(app?.child);if(upstream)await new Promise(resolve=>upstream.close(resolve));await fs.rm(dir,{recursive:true,force:true});
 }
}
main().catch(error=>{console.error(error.message);process.exitCode=1;});
