import html,re,sys
PAL=["#1e1e1e","#f14c4c","#23d18b","#f5f543","#3b8eea","#d670d6","#29b8db","#e5e5e5",
     "#666666","#f14c4c","#23d18b","#f5f543","#3b8eea","#d670d6","#29b8db","#ffffff"]
def c256(n):
    if n<16: return PAL[n]
    if n<232:
        n-=16; r,g,b=n//36,(n//6)%6,n%6
        return "#%02x%02x%02x"%tuple(0 if v==0 else 55+40*v for v in (r,g,b))
    v=8+10*(n-232); return "#%02x%02x%02x"%(v,v,v)
out=['<html><body style="margin:0;background:#1e1e1e"><pre style="margin:0;padding:16px;font:15px/1.35 \'DejaVu Sans Mono\',monospace;color:#e5e5e5;background:#1e1e1e;display:inline-block;min-width:100%">']
fg=bg=None;bold=italic=under=False
def span():
    st=[]
    if fg: st.append("color:"+fg)
    if bg: st.append("background:"+bg)
    if bold: st.append("font-weight:bold")
    if italic: st.append("font-style:italic")
    if under: st.append("text-decoration:underline")
    return '<span style="%s">'%";".join(st)
open_=False
for line in open(sys.argv[1],encoding="utf-8",errors="replace"):
    pos=0
    for m in re.finditer(r"\x1b\[([0-9;]*)m",line):
        out.append(html.escape(line[pos:m.start()])); pos=m.end()
        codes=[int(x) if x else 0 for x in m.group(1).split(";")]
        i=0
        while i<len(codes):
            c=codes[i]
            if c==0: fg=bg=None;bold=italic=under=False
            elif c==4: under=True
            elif c==24: under=False
            elif c==1: bold=True
            elif c==3: italic=True
            elif c in (22,23): bold=italic=False
            elif 30<=c<=37: fg=PAL[c-30]
            elif 90<=c<=97: fg=PAL[c-90+8]
            elif 40<=c<=47: bg=PAL[c-40]
            elif 100<=c<=107: bg=PAL[c-100+8]
            elif c==39: fg=None
            elif c==49: bg=None
            elif c in (38,48) and i+1<len(codes):
                if codes[i+1]==5 and i+2<len(codes):
                    col=c256(codes[i+2]); i+=2
                elif codes[i+1]==2 and i+4<len(codes):
                    col="#%02x%02x%02x"%tuple(codes[i+2:i+5]); i+=4
                else: col=None
                if c==38: fg=col
                else: bg=col
            i+=1
        if open_: out.append("</span>")
        out.append(span()); open_=True
    out.append(html.escape(line[pos:]))
if open_: out.append("</span>")
out.append("</pre></body></html>")
open(sys.argv[2],"w").write("".join(out))
