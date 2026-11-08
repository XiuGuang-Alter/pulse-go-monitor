// Retained history is bounded. Progress must use immutable check IDs and the
// process-local write counter, with an explicit measurement baseline.
class SoakHealth {
 constructor(){this.previous=null;this.previousWall=null;this.targets=new Map();this.completed=0;this.instanceID=null;this.baseline=null;this.lastWrites=null;}
 observe(seconds,targets,logs,status,wallSeconds=seconds){
  if(!Number.isFinite(seconds)||!Number.isFinite(wallSeconds))throw Error('Invalid sampling clock');
  if(this.previous!==null&&(seconds<this.previous||seconds-this.previous>30||Math.abs(wallSeconds-this.previousWall)>30))throw Error('Sampling gap exceeds 30 seconds');
  const dt=this.previous===null?0:seconds-this.previous;
  this.previous=seconds;this.previousWall=wallSeconds;
  if(/保存目标.*结果失败|读取.*失败|database.*(?:locked|error)|disk.*(?:full|error)/i.test(logs))throw Error('Background storage error during soak');
  const health=status?.monitor_health;
  if(!health||!['healthy','starting','idle'].includes(health.state)||health.consecutive_errors>0||health.affected_targets>0)throw Error('Monitor health failure: '+JSON.stringify(health));
  if(typeof health.instance_id!=='string'||!health.instance_id||!Number.isSafeInteger(health.successful_writes)||health.successful_writes<0)throw Error('Invalid monitor health counter or instance');
  if(this.instanceID===null){this.instanceID=health.instance_id;this.baseline=health.successful_writes;}
  if(this.instanceID!==health.instance_id)throw Error('Monitor instance changed during measurement; restart cannot resume this run');
  if(this.lastWrites!==null&&health.successful_writes<this.lastWrites)throw Error('Successful write counter regressed');
  this.lastWrites=health.successful_writes;this.completed=this.lastWrites-this.baseline;
  const live=new Set();
  for(const target of targets){
   live.add(target.id);
   const id=target.latest?.id??null,enabled=target.enabled!==false;
   let prior=this.targets.get(target.id);
   if(!prior)prior={id,age:0,grace:0,enabled,version:target.current_config_version};
   else if(id!==null&&id!==prior.id){
    if(prior.id!==null&&id<prior.id)throw Error('Latest check ID regressed for target '+target.id);
    prior.age=0;prior.grace=0;prior.id=id;
   }else{
    if(prior.enabled)prior.age+=dt;
    // Edits/resume receive one bounded allowance per actual check. Repeated
    // edits or toggles without a new ID cannot reset the starvation clock.
    if(enabled&&(!prior.enabled||target.current_config_version!==prior.version))prior.grace=Math.min(30,Math.max(5,Number(target.interval_seconds)||10));
   }
   prior.enabled=enabled;prior.version=target.current_config_version;
   if(enabled&&prior.age>=120+prior.grace)throw Error('No progress for target '+target.id+' for 120 active seconds (bounded configuration grace '+prior.grace+'s)');
   this.targets.set(target.id,prior);
  }
  for(const id of this.targets.keys())if(!live.has(id))this.targets.delete(id);
 }
}
module.exports={SoakHealth};
