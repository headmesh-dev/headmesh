cd /home/claude/brand/gen && sed -n 55,70p marks.py && python3 -c "
import sys; sys.path.insert(0,'.')
from marks import icon_svg; print(icon_svg('A1')[:900])"
