const $=id=>document.getElementById(id);
const esc=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const ago=t=>{const s=Math.max(0,(Date.now()-new Date(t))/1000|0);return s<60?s+'s':s<3600?(s/60|0)+'m':s<86400?(s/3600|0)+'h':(s/86400|0)+'d'};
const bytes=n=>n<1024?n+' B':n<1048576?(n/1024).toFixed(1)+' KiB':n<1073741824?(n/1048576).toFixed(1)+' MiB':(n/1073741824).toFixed(2)+' GiB';
function toast(t,bad){const m=$('msg');m.textContent=t;m.style.background=bad?'var(--bad)':'';m.style.display='block';clearTimeout(m._t);m._t=setTimeout(()=>m.style.display='none',3000)}
async function api(path,body){const r=await fetch(path,{method:body===undefined?'GET':'POST',headers:{'Content-Type':'application/json'},body:body===undefined?undefined:JSON.stringify(body)});
 const j=await r.json().catch(()=>({}));if(!r.ok)throw new Error(j.error||r.statusText);return j}
function table(head,rows){return rows.length?`<table><thead><tr>${head.map(h=>`<th>${h}</th>`).join('')}</tr></thead><tbody>${rows.join('')}</tbody></table>`:'<div class="empty">None</div>'}
function nodeRow(tbody,n={}){const tr=document.createElement('tr');tr.innerHTML=`<td><input placeholder="name" value="${esc(n.name)}"></td><td><input placeholder="10.0.0.5:22" value="${esc(n.addr)}"></td><td><input placeholder="optional" value="${esc(n.desc)}"></td><td><button class="danger" onclick="this.closest('tr').remove()">✕</button></td>`;tbody.appendChild(tr)}
function readNodes(tbody){return [...tbody.querySelectorAll('tr')].map(tr=>{const i=tr.querySelectorAll('input');return{name:i[0].value.trim(),addr:i[1].value.trim(),desc:i[2].value.trim()}}).filter(n=>n.name||n.addr)}
function stateCell(s){return `<span class="state-${esc(s.state)}">${esc(s.state)}</span>${s.error?`<div class="muted">${esc(s.error)}</div>`:''}`}
const js=v=>esc(JSON.stringify(v));
// A secret shows as dots until its Show button is pressed; shown keeps which
// ones are shown across renders. The value and the dots, and Show and Hide,
// take the same space, the wider one's, so the table stays put on a toggle.
const shown=new Set();
function secret(key,v){const on=shown.has(key)?' on':'';return `<span class="secret swap${on}"><span>••••••••</span><span>${esc(v)}</span></span><button class="eye swap${on}" onclick="toggleSecret(${js(key)})"><span>Show</span><span>Hide</span></button>`}
function toggleSecret(key){shown.has(key)?shown.delete(key):shown.add(key);render()}
// pageURL is the http URL of a listen address; one on every address is
// reached at the host this page came from.
function pageURL(addr){const m=String(addr).match(/^(.*):(\d+)$/);let h=(m?m[1]:addr).replace(/^\[|\]$/g,'');
 if(!h||h==='0.0.0.0'||h==='::')h=location.hostname.replace(/^\[|\]$/g,'');return 'http://'+(h.includes(':')?'['+h+']':h)+(m?':'+m[2]:'')}
// openPage opens the page at addr in a new tab, logged in with auth,
// "user:password", by posting it to the page's /login.
function openPage(addr,auth){const url=pageURL(addr);if(!auth){window.open(url+'/','_blank','noopener');return}
 const i=auth.indexOf(':'),f=document.createElement('form');f.method='POST';f.action=url+'/login';f.target='_blank';f.rel='noopener';
 for(const[k,v]of[['user',i<0?auth:auth.slice(0,i)],['password',i<0?'':auth.slice(i+1)]]){const e=document.createElement('input');e.type='hidden';e.name=k;e.value=v;f.appendChild(e)}
 document.body.appendChild(f);f.submit();f.remove()}
// loginCells are the user, password and Open cells of the page at addr.
function loginCells(key,addr,auth){const i=(auth||'').indexOf(':'),user=i<0?auth:auth.slice(0,i),pass=i<0?'':auth.slice(i+1);
 return `<td>${auth?esc(user):'<span class="muted">no login</span>'}</td><td>${auth?secret(key,pass):''}</td><td><button class="primary" onclick="openPage(${js(addr)},${js(auth||'')})">Open</button></td>`}
