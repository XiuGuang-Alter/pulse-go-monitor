const {spawn}=require('node:child_process');
const {once}=require('node:events');
async function stopChild(child){
 if(!child||!child.pid||child.exitCode!==null||child.signalCode!==null)return;
 const exited=once(child,'exit');
 child.kill('SIGKILL');
 await exited;
}
async function launchServer(exe,args,{timeout=10000}={}){
 const child=spawn(exe,args,{windowsHide:true,stdio:['ignore','pipe','pipe']});
 let logs='',output='',timer;
 child.stderr.on('data',chunk=>logs+=chunk);
 try{
  const url=await new Promise((resolve,reject)=>{
   timer=setTimeout(()=>reject(new Error('Startup timeout: '+logs)),timeout);
   child.stdout.on('data',chunk=>{output+=chunk;const match=output.match(/http:\/\/127\.0\.0\.1:\d+/);if(match)resolve(match[0]);});
   child.once('error',reject);
   child.once('exit',code=>reject(new Error('Early exit '+code+': '+logs)));
  });
  return {child,url};
 }catch(error){await stopChild(child);throw error;}
 finally{clearTimeout(timer);}
}
module.exports={launchServer,stopChild};
