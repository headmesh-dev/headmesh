s=open('build.py').read()
s=s.replace("mx_, my_ = (left[0] + right[0]) / 2, (left[1] + right[1]) / 2 - fs * 1.5",
            "mx_ = x + wa + gap / 2\n        my_ = left[1] + (right[1] - left[1]) * ((mx_ - left[0]) / (right[0] - left[0])) - fs * 1.4")
open('build.py','w').write(s)
