const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs'),path=require('node:path'),vm=require('node:vm');
const {EventEmitter}=require('node:events');
const root=path.resolve(__dirname,'..');
// Execute the real entry point, replacing only OS/HTTP/time boundaries. A helper
// test alone cannot prove that the experiment actually invokes its safeguards.
async function experiment(mode='healthy',extraEnv={},hostFS=fs){
 let clock=0,wallJump=0,reads=0,launched=0,stopped=0,nextID=1,upstreamHandler;const written=new Map(),targets=[],routes=[];
 const child={pid:123,stderr:new EventEmitter()};
 const upstream=new EventEmitter();upstream.listen=()=>queueMicrotask(()=>upstream.emit('listening'));
 upstream.address=()=>({port:18001});upstream.closeAllConnections=()=>{};upstream.close=cb=>cb();
 const output=path.join(root,'.cache','synthetic-soak-test');
 const cache=path.join(root,'.cache'),directories=new Set(mode==='existing'?[output]:[]);
 const inCache=file=>{const relative=path.relative(cache,path.resolve(file));return relative===''||relative!=='..'&&!relative.startsWith('..'+path.sep)&&!path.isAbsolute(relative);};
 const exists=file=>{const resolved=path.resolve(file);return directories.has(resolved)||(inCache(resolved)?false:hostFS.existsSync(resolved));};
 function missing(file){const error=Error('Virtual directory does not exist: '+file);error.code='ENOENT';return error;}
 const fakeProcess={pid:456,platform:'win32',arch:'x64',version:process.version,exitCode:0,env:{PULSE_SOAK_SECONDS:mode==='toggle starves'?'300':'180',PULSE_SOAK_MUTATION_SECONDS:'40',PULSE_SOAK_OUT:output,...extraEnv},on(){},removeListener(){}};
 // Cache directories belong entirely to this fixture. Their existence and
 // canonical paths must agree after mkdir, even on a clean CI checkout.
 const files={...fs,existsSync:exists,mkdirSync(file,options={}){
   const resolved=path.resolve(file),parent=path.dirname(resolved);
   if(exists(resolved)){if(options.recursive)return;const error=Error('Virtual directory exists: '+resolved);error.code='EEXIST';throw error;}
   if(!exists(parent)){if(!options.recursive)throw missing(parent);files.mkdirSync(parent,options);}
   directories.add(resolved);
  },
  realpathSync(file){const resolved=path.resolve(file);if(!exists(resolved))throw missing(resolved);if(mode==='symlink escape'&&inCache(resolved))return path.resolve(root,'..');if(directories.has(resolved))return path.join(hostFS.realpathSync(root),path.relative(root,resolved));return hostFS.realpathSync(resolved);},
  readFileSync(file,...args){if(String(file).endsWith('pulse-soak.exe'))return Buffer.from('fixture binary');return fs.readFileSync(file,...args);},
  writeFileSync(file,data){if(mode==='progress failure'&&String(file).includes('progress.json')&&String(data).includes('cleaning'))throw Error('fixture progress write failed');written.set(path.basename(file),String(data));},
  renameSync(from,to){written.set(path.basename(to),written.get(path.basename(from)));written.delete(path.basename(from));},
  appendFileSync(file,data){written.set(path.basename(file),(written.get(path.basename(file))||'')+data);},
  statSync(file){if(String(file).includes('synthetic.db'))return {size:4096};return fs.statSync(file);}};
 const launch={async launchServer(){launched++;return {child,url:'http://127.0.0.1:18000'};},async stopChild(){stopped++;if(mode==='cleanup failure'&&stopped===2)throw Error('fixture cleanup failed');}};
 const fakeFetch=async(url,options={})=>{
  const route=new URL(url).pathname,method=options.method||'GET';routes.push(method+' '+route);
  let value,code=200;
  if(route==='/api/targets'&&method==='POST'){
   value={...JSON.parse(options.body),id:nextID++,current_config_version:1,stats:{samples:1000},latest:null};targets.push(value);
  }else if(route==='/api/status'){
   if(mode==='overlap'&&reads===0){upstreamHandler({url:'/same'},new EventEmitter());upstreamHandler({url:'/same'},new EventEmitter());}
   if(mode==='write failure')child.stderr.emit('data','保存目标 1 结果失败: disk full');
   value={running:mode==='slots'?5:0,heap_alloc_bytes:1024,goroutines:10,server_time:Date.now(),monitor_health:{state:mode==='degraded'?'degraded':'healthy',instance_id:mode==='instance'&&reads>3?'unexpected':String(launched),successful_writes:mode==='rollback'&&reads>3?0:500+reads*100,consecutive_errors:0,affected_targets:0}};
   if(mode==='api failure')code=500;
  }else if(route==='/api/targets'){
   reads++;
   value=targets.map((t,i)=>{
    const stalled=mode==='stalled'||(mode==='one starves'||mode==='toggle starves')&&t.id===98||mode==='edited starves'&&t.id===2;
    const paused=mode==='pause'&&t.id===98&&reads>=2&&reads<=15;
    const latest={id:stalled?t.id:paused?(t.latest?.id??t.id):reads*1000+t.id,kind:['assertion_failed','http_error','timeout','ok'][Math.min(i,3)]};
    if(mode==='toggle starves'&&t.id===98)t.enabled=reads%2===1;
    if(mode==='pause'&&t.id===98)t.enabled=!(reads>=2&&reads<=15);
    if(mode==='edited starves'&&t.id===2)t.current_config_version++;
    t.latest=latest;if(mode==='restart lost config'&&launched===2&&t.id===98)t.name='lost configuration';return {...t,stats:{samples:1000},next_check_at:Date.now()-10};
   });
  }else if(/^\/api\/targets\/\d+$/.test(route)){
   const id=Number(route.split('/').pop()),idx=targets.findIndex(t=>t.id===id);
   if(method==='DELETE'){targets.splice(idx,1);code=204;}
   else if(method==='PUT'){Object.assign(targets[idx],JSON.parse(options.body));targets[idx].current_config_version++;value=targets[idx];}
   else if(idx<0)code=404;else value=targets[idx];
  }else if(/\/checks$|\/daily$/.test(route)){
   const id=Number(route.split('/')[3]);if(!targets.some(t=>t.id===id))code=mode==='resurrected'?200:404;
   value=route.endsWith('/checks')?{items:[]}:{days:[]};
  }else throw Error('Unexpected fixture route '+route);
  return {ok:code>=200&&code<300,status:code,json:async()=>structuredClone(value)};
 };
 class ClockDate extends Date{constructor(...args){super(...(args.length?args:[1700000000000+clock+wallJump]));}static now(){return 1700000000000+clock+wallJump;}}
 const modules={
  'node:fs':files,'node:path':path,'node:http':{createServer:handler=>{upstreamHandler=handler;return upstream;}},
  'node:child_process':{spawnSync:()=>({status:0,stdout:'1024',stderr:''})},
  'node:events':require('node:events'),'node:crypto':require('node:crypto'),
  'node:timers/promises':{setTimeout:async(ms)=>{clock+=ms;if(mode==='sleep'&&clock>=30000)wallJump+=60000;if(mode==='mono gap'&&clock>=30000)clock+=40000;}},
  'node:perf_hooks':{performance:{now:()=>clock}},
  './launch-server.cjs':launch,'./soak-health.cjs':require('./soak-health.cjs'),
 };
 const source=fs.readFileSync(process.env.PULSE_SOAK_TEST_SOURCE||path.join(__dirname,'soak.cjs'),'utf8');
 await vm.runInNewContext(source,{require:name=>{if(!(name in modules))throw Error('Unexpected module '+name);return modules[name];},__dirname,process:fakeProcess,setTimeout:()=>0,clearTimeout(){},Date:ClockDate,fetch:fakeFetch,AbortSignal:{timeout:()=>null},console:{log(){},error(){}}},{filename:'actual-soak.cjs'});
 return {written,exitCode:fakeProcess.exitCode,launched,stopped,routes};
}
for(const [mode,pattern] of [['stalled',/progress/],['one starves',/progress/],['edited starves',/progress/],['toggle starves',/progress/],['write failure',/storage/],['degraded',/health/],['api failure',/HTTP500/],['instance',/instance/],['rollback',/counter/],['sleep',/gap/],['mono gap',/gap/],['slots',/Correctness/],['overlap',/Correctness/],['resurrected',/Deleted/]])test('actual soak rejects '+mode+' and records terminal failure',async()=>{
 const r=await experiment(mode);assert.equal(r.exitCode,1);assert.match(r.written.get('failure.txt'),pattern);
 assert.equal(JSON.parse(r.written.get('result.json')).passed,false);assert.equal(JSON.parse(r.written.get('progress.json')).phase,'failed');assert.ok(r.stopped>=1,'child cleanup must run');
});
for(const mode of ['healthy','pause'])test('actual soak accepts capped history with latest progress: '+mode,async()=>{
 const r=await experiment(mode);assert.equal(r.exitCode,0,r.written.get('failure.txt'));
 const result=JSON.parse(r.written.get('result.json'));assert.equal(result.passed,true);assert.ok(result.completed_writes>=1000);assert.equal(result.segments.length,1);assert.equal(result.restart.passed,true);assert.notEqual(result.restart.instance_id,result.segments[0].instance_id);
 assert.ok(r.routes.some(r=>r.includes('/daily')));assert.ok(r.routes.some(r=>r.startsWith('PUT ')));assert.ok(r.routes.some(r=>r.startsWith('DELETE ')));assert.match(r.written.get('samples.csv'),/db_bytes,wal_bytes,shm_bytes/);assert.equal(JSON.parse(r.written.get('progress.json')).phase,'completed');
});
test('restart persistence failure has its own failed record',async()=>{const r=await experiment('restart lost config');assert.equal(r.exitCode,1);assert.equal(JSON.parse(r.written.get('result.json')).passed,false);const restart=JSON.parse(r.written.get('restart.json'));assert.equal(restart.passed,false);assert.match(restart.error,/Persistence/);});
test('progress record I/O failure still cleans up and cannot pass',async()=>{const r=await experiment('progress failure');assert.ok(r.stopped>=2);assert.equal(r.exitCode,1);assert.equal(JSON.parse(r.written.get('result.json')).passed,false);});
test('cleanup failure cannot publish passed even after restart succeeds',async()=>{const r=await experiment('cleanup failure');assert.equal(r.exitCode,1);const result=JSON.parse(r.written.get('result.json'));assert.equal(result.passed,false);assert.equal(result.cleanup.passed,false);assert.equal(result.restart.passed,true);assert.match(r.written.get('failure.txt'),/cleanup/);});
test('existing evidence directory is refused before launch',async()=>{await assert.rejects(()=>experiment('existing'),/exists|existing/);});




test('lexical parent path escape is refused before launch',async()=>{await assert.rejects(()=>experiment('healthy',{PULSE_SOAK_OUT:path.resolve(root,'..','escaped-evidence')}),/stay in project/);});
test('resolved junction/symlink parent escape is refused before launch',async()=>{await assert.rejects(()=>experiment('symlink escape'),/outside project/);});

test('fresh checkout creates virtual evidence parents without a host cache directory',async()=>{
 const cache=path.join(root,'.cache');
 const isCache=file=>{const rel=path.relative(cache,path.resolve(file));return rel===''||rel!=='..'&&!rel.startsWith('..'+path.sep)&&!path.isAbsolute(rel);};
 const hostFS={...fs,existsSync(file){return isCache(file)?false:fs.existsSync(file);},realpathSync(file){if(isCache(file)){const error=Error('Host cache does not exist: '+file);error.code='ENOENT';throw error;}return fs.realpathSync(file);}};
 const r=await experiment('healthy',{},hostFS);
 assert.equal(r.exitCode,0,r.written.get('failure.txt'));
 assert.equal(JSON.parse(r.written.get('result.json')).passed,true);
});
