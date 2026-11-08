// Real v1.1 (schema3) -> v1.2 (schema4) -> rejection -> untouched backup recovery.
// Arguments are user-provided binaries; all data is synthetic and temporary.
const fs=require('node:fs/promises'),os=require('node:os'),path=require('node:path'),http=require('node:http');
const {once}=require('node:events');
const {setTimeout:delay}=require('node:timers/promises');
const {createHash}=require('node:crypto');
const {DatabaseSync}=require('node:sqlite');
function schema(folder){const db=new DatabaseSync(path.join(folder,'monitor.db'),{readOnly:true});try{return db.prepare('PRAGMA user_version').get().user_version;}finally{db.close();}}
const {launchServer,stopChild}=require('./launch-server.cjs');
async function main(){
 const [oldBinary,newBinary]=process.argv.slice(2).map(p=>path.resolve(p));
 if(!oldBinary||!newBinary)throw Error('Pass v1.1 and candidate binary paths');
 const dir=await fs.mkdtemp(path.join(os.tmpdir(),'pulse-upgrade-'));let app,upstream;
 const live=path.join(dir,'live'),backup=path.join(dir,'backup'),restored=path.join(dir,'restored');
 await fs.mkdir(live);
 const launch=(binary,folder)=>launchServer(binary,['serve','-addr','127.0.0.1:0','-db',path.join(folder,'monitor.db')]);
 const api=async(route,method='GET',data)=>{const response=await fetch(app.url+route,{method,headers:{'Content-Type':'application/json'},body:data?JSON.stringify(data):undefined});if(!response.ok)throw Error('API '+response.status);return response.json();};
 try{
  upstream=http.createServer((req,res)=>res.end('upgrade synthetic fixture'));upstream.listen(0,'127.0.0.1');await once(upstream,'listening');
  app=await launch(oldBinary,live);
  const target=await api('/api/targets','POST',{name:'upgrade-fixture',url:'http://127.0.0.1:'+upstream.address().port,enabled:false,interval_seconds:60,timeout_seconds:1});
  await api('/api/targets/'+target.id+'/check','POST');
  let original;for(let i=0;i<100;i++){original=await api('/api/targets/'+target.id+'/checks');if(original.items.length)break;await delay(50);}
  if(original.items.length!==1)throw Error('Missing first-version history');
  await stopChild(app.child);if(schema(live)!==3)throw Error("Baseline must be actual schema3");await fs.cp(live,backup,{recursive:true});
  const hash=async(folder)=>{const h=createHash('sha256');for(const name of (await fs.readdir(folder)).sort()){h.update(name);h.update(await fs.readFile(path.join(folder,name)));}return h.digest('hex');};
  const backupHash=await hash(backup);
  app=await launch(newBinary,live);
  const targets=await api('/api/targets'),history=await api('/api/targets/'+target.id+'/checks'),daily=await api('/api/targets/'+target.id+'/daily?days=1');
  if(targets[0].id!==target.id||history.items[0].id!==original.items[0].id||history.items[0].total_duration_ms!==original.items[0].total_duration_ms||history.items[0].config_version!==original.items[0].config_version||JSON.stringify(history.items[0].rules_snapshot)!==JSON.stringify(original.items[0].rules_snapshot)||history.items[0].error_category!==''||daily.buckets[0].samples!==1||daily.buckets[0].migration_partial)throw Error('Migration did not preserve legacy semantics');
  await stopChild(app.child);if(schema(live)!==4)throw Error("Candidate must migrate to schema4");
  let refused=false;try{app=await launch(oldBinary,live);}catch(error){refused=/版本|version/i.test(String(error));}
  if(!refused)throw Error('Old program did not reject upgraded schema');
  if(await hash(backup)!==backupHash)throw Error('Backup changed');
  await fs.cp(backup,restored,{recursive:true});app=await launch(oldBinary,restored);
  const rollback=await api('/api/targets/'+target.id+'/checks');if(rollback.items[0].id!==original.items[0].id)throw Error('Restored history differs');
  console.log(JSON.stringify({passed:true,upgrade:'actual v1.1 binary database schema3 -> schema4',target_id_preserved:true,history_id_preserved:true,old_binary_refused_upgraded:true,restored_backup_opened_by_v11:true,backup_sha256:backupHash}));
 }finally{await stopChild(app?.child);if(upstream)await new Promise(resolve=>upstream.close(resolve));await fs.rm(dir,{recursive:true,force:true});}
}
main().catch(error=>{console.error(error);process.exitCode=1;});
