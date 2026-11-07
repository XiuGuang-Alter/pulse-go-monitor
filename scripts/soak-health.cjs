// Bound the experiment's blind spots: elapsed time alone is not evidence of work.
class SoakHealth {
 constructor(){this.previous=null;this.targets=new Map();}
 observe(seconds,targets,logs){
  if(this.previous!==null&&seconds-this.previous>30)throw Error('Sampling gap exceeds 30 seconds');
  this.previous=seconds;
  if(/保存目标.*结果失败|读取.*失败|database.*(?:locked|error)|disk.*(?:full|error)/i.test(logs))throw Error('Background storage error during soak');
  const live=new Set();
  for(const target of targets){
   live.add(target.id);
   const count=target.stats.samples;
   let prior=this.targets.get(target.id);
   if(!prior||count>prior.count)prior={count,changed:seconds};
   if(seconds-prior.changed>=120)throw Error('No progress for target '+target.id+' for 120 seconds');
   this.targets.set(target.id,prior);
  }
  for(const id of this.targets.keys())if(!live.has(id))this.targets.delete(id);
 }
}
module.exports={SoakHealth};
