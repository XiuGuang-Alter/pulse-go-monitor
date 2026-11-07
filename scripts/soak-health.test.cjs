const {test}=require('node:test');
const assert=require('node:assert/strict');
const {SoakHealth}=require('./soak-health.cjs');
const targets=(samples=0)=>Array.from({length:100},(_,id)=>({id,stats:{samples}}));
test('soak rejects globally stalled and never checked targets',()=>{
 const guard=new SoakHealth();guard.observe(0,targets(), '');
 const partial=targets();for(let i=0;i<4;i++)partial[i].stats.samples=1;
 for(let s=10;s<=110;s+=10)guard.observe(s,partial,'');
 assert.throws(()=>guard.observe(120,partial,''),/progress/);
});
test('soak rejects one stalled target while others progress',()=>{
 const guard=new SoakHealth();guard.observe(0,targets(), '');
 for(let s=10;s<=110;s+=10){const t=targets(s);t[99].stats.samples=0;guard.observe(s,t,'');}
 const t=targets(120);t[99].stats.samples=0;
 assert.throws(()=>guard.observe(120,t,''),/progress/);
});
test('soak fails on storage errors and suspended sampling',()=>{
 assert.throws(()=>new SoakHealth().observe(0,targets(),'保存目标 1 结果失败: disk full'),/storage/);
 assert.throws(()=>new SoakHealth().observe(0,targets(),'读取到期目标失败: disk error'),/storage/);
 const guard=new SoakHealth();guard.observe(0,targets(),'');assert.throws(()=>guard.observe(45,targets(1),''),/gap/);
});
test('soak accepts healthy progress, deleted IDs and replacement grace',()=>{
 const guard=new SoakHealth();for(let s=0;s<=300;s+=10)guard.observe(s,targets(s),'');
 const replacement=targets(301);replacement[99]={id:100,stats:{samples:0}};guard.observe(310,replacement,'');
});
