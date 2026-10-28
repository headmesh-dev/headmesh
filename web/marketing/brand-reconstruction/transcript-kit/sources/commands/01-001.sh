python3 gen/preview.py && node gen/render.js preview/marks.html preview/marks.png 1650 1200 1 && python3 -c "
from PIL import Image; im=Image.open('preview/marks.png'); print(im.size)"
