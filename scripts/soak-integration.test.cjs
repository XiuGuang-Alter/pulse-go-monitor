const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs'),path=require('node:path'),vm=require('node:vm');
const {EventEmitter}=require('node:events');
const root=path.resolve(__dirname,'..');

// Execute the real experiment script; replace only the OS/HTTP/time boundaries.
// This must fail if the script stops calling its health checks.
async function experiment(mode){
 let clock=0,reads=0;const written=new Map(),targets=[];
 const child={pid:123,stderr:new EventEmitter()};
 const upstream=new EventEmitter();
 upstream.listen=()=>queueMicrotask(()=>upstream.emit('listening'));
 upstream.address=()=>({port:18001});upstream.closeAllConnections=()=>{};upstream.close=cb=>cb();
 const fakeProcess={platform:'win32',arch:'x64',version:process.version,exitCode:0,env:{PULSE_SOAK_SECONDS:'180',PULSE_SOAK_OUT:path.join(root,'.cache','synthetic-soak-test')}};
 const files={...fs,mkdirSync(){},writeFileSync(file,data){written.set(path.basename(file),String(data));},appendFileSync(file,data){written.set(path.basename(file),(written.get(path.basename(file))||'')+data);}};
 const launch={async launchServer(){return {child,url:'http://127.0.0.1:18000'};},async stopChild(){}};
 const fakeFetch=async(url,options={})=>{
  const route=new URL(url).pathname;
  let value;
  if(route==='/api/targets'&&options.method==='POST'){
   value={...JSON.parse(options.body),id:targets.length+1,stats:{samples:0}};targets.push(value);
  }else if(route==='/api/status'){
   if(mode==='write failure')child.stderr.emit('data','保存目标 1 结果失败: disk full');
   value={running:0,heap_alloc_bytes:1024,goroutines:10};
  }else if(route==='/api/targets'){
   reads++;
   value=targets.map((target,i)=>({...target,stats:{samples:mode==='stalled'?Number(i<4):mode==='one starves'&&i===99?0:reads},latest:i<4?{kind:['ok','http_error','timeout','assertion_failed'][i]}:null}));
  }else throw Error('Unexpected fixture route '+route);
  return {ok:true,status:200,json:async()=>structuredClone(value)};
 };
 const modules={
  'node:fs':files,'node:path':path,'node:http':{createServer:()=>upstream},
  'node:child_process':{spawnSync:()=>({status:0,stdout:'1024'})},
  'node:events':require('node:events'),'node:crypto':require('node:crypto'),
  'node:timers/promises':{setTimeout:async(ms)=>{clock+=ms;}},
  'node:perf_hooks':{performance:{now:()=>clock}},
  './launch-server.cjs':launch,'./soak-health.cjs':require('./soak-health.cjs'),
 };
 const source=fs.readFileSync(process.env.PULSE_SOAK_TEST_SOURCE||path.join(__dirname,'soak.cjs'),'utf8');
 await vm.runInNewContext(source,{require:name=>{if(!(name in modules))throw Error('Unexpected module '+name);return modules[name];},__dirname,process:fakeProcess,fetch:fakeFetch,AbortSignal:{timeout:()=>null},console:{log(){},error(){}}},{filename:'actual-soak.cjs'});
 return {written,exitCode:fakeProcess.exitCode};
}
for(const mode of ['stalled','one starves','write failure'])test('actual soak script rejects '+mode,async()=>{
 const result=await experiment(mode);
 assert.equal(result.exitCode,1,'unhealthy experiment was allowed to pass');
 assert.equal(result.written.has('result.json'),false,'must not emit passed result');
 assert.match(result.written.get('failure.txt'),mode==='write failure'?/storage/:/progress/);
});
