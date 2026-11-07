// Check the Git index so ignored local materials remain private even during development.
const {execFileSync}=require('node:child_process');
const privatePath=/(?:学习笔记|教学|学习记录|教程)|(?:^|\/)(?:tutorials?|learning[-_]notes)(?:\/|[._-]|$)/i;
const files=execFileSync('git',['ls-files','--cached','-z'],{encoding:'utf8'}).split('\0').filter(Boolean);
const errors=[];
for(const file of files){
 if(privatePath.test(file))errors.push('Private material tracked: '+file);
 if(file.endsWith('.md')){
  const contents=execFileSync('git',['show',':'+file],{encoding:'utf8'});
  for(const match of contents.matchAll(/\]\(([^)]+)\)|^\s{0,3}\[[^\]]+\]:\s*<?([^\s>]+)>?|\b(?:href|src)\s*=\s*["']([^"']+)["']/gmi)){
   const target=decodeURIComponent(match[1]??match[2]??match[3]);
   if(privatePath.test(target))errors.push('Private material linked: '+file+' -> '+target);
  }
 }
}
if(errors.length){console.error(errors.join('\n'));process.exitCode=1;}
else console.log('PASS: public index excludes teaching materials and links');
