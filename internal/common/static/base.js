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
