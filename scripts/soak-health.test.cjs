const {test}=require('node:test');
const assert=require('node:assert/strict');
const {SoakHealth}=require('./soak-health.cjs');
const status=(writes=0,instance='one')=>({monitor_health:{state:'healthy',instance_id:instance,successful_writes:writes,consecutive_errors:0,affected_targets:0}});
const targets=(id=0)=>Array.from({length:100},(_,i)=>({id:i+1,enabled:true,current_config_version:1,interval_seconds:10,stats:{samples:1000},latest:id?{id:id*100+i+1}:null}));
function sample(g,s,t=targets(s),st=status(s),wall=s){return g.observe(s,t,'',st,wall);}
test('retained history cap does not hide advancing latest IDs and writes',()=>{
 const g=new SoakHealth();for(let s=0;s<=300;s+=10)sample(g,s);assert.equal(g.completed,300);
});
test('counter baseline excludes writes before the measurement',()=>{
 const g=new SoakHealth();sample(g,0,targets(1),status(800));sample(g,10,targets(2),status(850));assert.equal(g.completed,50);
});
for(const mode of ['all','one'])test('latest IDs detect '+mode+' stalled targets despite capped history',()=>{
 const g=new SoakHealth();sample(g,0,targets(1));
 for(let s=10;s<=110;s+=10){const t=targets(s+1);for(let i=0;i<(mode==='all'?100:1);i++)t[i].latest={id:101+i};sample(g,s,t);}
 const t=targets(200);t[0].latest={id:101};assert.throws(()=>sample(g,120,t),/progress/);
});
test('pause excludes inactive time and resume has bounded grace',()=>{
 const g=new SoakHealth();let t=targets(1);sample(g,0,t);
 for(let s=10;s<=100;s+=10)sample(g,s,t,status(s));
 t[0].enabled=false;for(let s=110;s<=400;s+=10){const rest=targets(s);rest[0]=t[0];sample(g,s,rest,status(s));}
 t[0].enabled=true;t[0].current_config_version=3;
 for(let s=410;s<=430;s+=10){const rest=targets(s);rest[0]=t[0];if(s===430)assert.throws(()=>sample(g,s,rest,status(s)),/progress/);else sample(g,s,rest,status(s));}
});
test('repeated configuration edits cannot indefinitely forgive starvation',()=>{
 const g=new SoakHealth();sample(g,0,targets(1));
 for(let s=10;s<=130;s+=10){const t=targets(s+1);t[0].latest={id:101};t[0].current_config_version=s;if(s===130)assert.throws(()=>sample(g,s,t),/progress/);else sample(g,s,t);}
});
test('deleted IDs have independent replacement grace',()=>{
 const g=new SoakHealth();for(let s=0;s<=130;s+=10)sample(g,s);const t=targets(140);t[99]={...t[99],id:101,latest:null};sample(g,140,t);
});
for(const mode of ['degraded','instance','rollback'])test('health rejects '+mode,()=>{
 const g=new SoakHealth();sample(g,0,targets(1),status(10));const st=status(mode==='rollback'?9:20,mode==='instance'?'two':'one');if(mode==='degraded')st.monitor_health.state='degraded';assert.throws(()=>sample(g,10,targets(2),st),/health|instance|counter/);
});
test('storage failures and monotonic or wall-clock sampling gaps fail',()=>{
 assert.throws(()=>new SoakHealth().observe(0,targets(),'保存目标 1 结果失败: disk full',status()),/storage/);
 for(const [mono,wall] of [[45,45],[10,100],[40,10]]){const g=new SoakHealth();sample(g,0);assert.throws(()=>sample(g,mono,targets(1),status(1),wall),/gap/);}
});

test('repeated pause and resume cannot erase already enabled time',()=>{const g=new SoakHealth();sample(g,0,targets(1));for(let s=10;s<=260;s+=10){const t=targets(s+1);t[0].latest={id:101};t[0].enabled=s%20===0;t[0].current_config_version=s;if(s===260)assert.throws(()=>sample(g,s,t),/progress/);else sample(g,s,t);}});
