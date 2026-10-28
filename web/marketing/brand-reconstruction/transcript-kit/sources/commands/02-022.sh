python3 -c "
from PIL import Image; im=Image.open('preview/kit.png').convert('RGB'); w,h=im.size
im.crop((0,0,w,h//2)).save('preview/kit_a.jpg',quality=80); im.crop((0,h//2,w,h)).save('preview/kit_b.jpg',quality=80)"
