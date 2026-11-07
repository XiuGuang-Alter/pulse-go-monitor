const {test}=require('node:test');
const assert=require('node:assert/strict');
const {spawnSync}=require('node:child_process');
const fs=require('node:fs');
const os=require('node:os');
const path=require('node:path');
const checker=path.resolve(__dirname,'check-public-files.cjs');
for(const [name,file,contents,expected] of [
 ['product documentation','README.md','See [architecture](docs/架构.md).',0],
 ['teaching filename','Go教学.md','private',1],
 ['inline link','README.md','[notes](学习笔记/00.md)',1],
 ['reference link','README.md','[notes][guide]\n\n[guide]: 学习笔记/00.md',1],
 ['HTML link','README.md','<a href="学习笔记/00.md">notes</a>',1],
 ['encoded link','README.md','[notes]('+encodeURIComponent('学习笔记')+'/00.md)',1],
])test('public boundary: '+name,()=>{
 const directory=fs.mkdtempSync(path.join(os.tmpdir(),'pulse-public-index-'));
 try{
  const git=(...args)=>{const result=spawnSync('git',args,{cwd:directory,encoding:'utf8'});assert.equal(result.status,0,result.stderr);};
  git('init','--quiet');fs.writeFileSync(path.join(directory,file),contents);git('add','--',file);
  const result=spawnSync(process.execPath,[checker],{cwd:directory,encoding:'utf8'});
  assert.equal(result.status,expected,result.stdout+result.stderr);
 }finally{fs.rmSync(directory,{recursive:true,force:true});}
});
