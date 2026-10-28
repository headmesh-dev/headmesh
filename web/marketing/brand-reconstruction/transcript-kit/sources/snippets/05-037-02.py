import json
jobs=json.load(open('tmp/kit-jobs.json'))
sel=[j for j in jobs if ('D4-shared' in j['in'] or 'C1-weave' in j['in']) and ('og-' in j['out'] or 'banner-x' in j['out'] or 'linkedin' in j['out'])]
json.dump(sel,open('tmp/sel.json','w'))
